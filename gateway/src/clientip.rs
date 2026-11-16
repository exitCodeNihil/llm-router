//! The client address a usage event names (LLMR_USAGE_CLIENT_IP): the
//! connecting address, or the one a trusted proxy says it forwarded for
//! (LLMR_TRUSTED_PROXIES). The rule is the control plane's
//! (internal/console/ratelimit.go, clientAddr and parseHop).

use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::sync::OnceLock;

use http::HeaderMap;

/// An address block: `bits` leading bits of `net`, which is masked to them.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Prefix {
    net: IpAddr,
    bits: u8,
}

fn unmap(a: IpAddr) -> IpAddr {
    match a {
        IpAddr::V6(v6) => v6.to_ipv4_mapped().map_or(a, IpAddr::V4),
        v4 => v4,
    }
}

fn masked(a: IpAddr, bits: u8) -> IpAddr {
    match a {
        IpAddr::V4(a) => {
            let mask = u32::MAX.checked_shl(32 - bits as u32).unwrap_or(0);
            IpAddr::V4(Ipv4Addr::from(u32::from(a) & mask))
        }
        IpAddr::V6(a) => {
            let mask = u128::MAX.checked_shl(128 - bits as u32).unwrap_or(0);
            IpAddr::V6(Ipv6Addr::from(u128::from(a) & mask))
        }
    }
}

impl Prefix {
    /// An IPv4 block holds no IPv6 address and the other way around; callers
    /// unmap IPv4-in-IPv6 addresses first.
    fn contains(&self, a: IpAddr) -> bool {
        self.net.is_ipv4() == a.is_ipv4() && masked(a, self.bits) == self.net
    }
}

/// LLMR_TRUSTED_PROXIES: addresses and CIDR blocks, comma separated
/// (config.parsePrefixes).
pub fn parse_trusted(list: &str) -> Result<Vec<Prefix>, String> {
    let mut out = Vec::new();
    for f in list.split(',').map(str::trim).filter(|f| !f.is_empty()) {
        let bad = || format!("{f:?} is not an address or a CIDR block");
        let prefix = match f.split_once('/') {
            None => {
                let a = unmap(f.parse::<IpAddr>().map_err(|_| bad())?);
                Prefix { net: a, bits: if a.is_ipv4() { 32 } else { 128 } }
            }
            Some((addr, bits)) => {
                let a = addr.parse::<IpAddr>().map_err(|_| bad())?;
                let plain = !bits.is_empty()
                    && bits.bytes().all(|b| b.is_ascii_digit())
                    && (bits.len() == 1 || !bits.starts_with('0'));
                let max = if a.is_ipv4() { 32 } else { 128 };
                let bits = bits.parse::<u8>().ok().filter(|&n| plain && n <= max).ok_or_else(bad)?;
                Prefix { net: masked(a, bits), bits }
            }
        };
        out.push(prefix);
    }
    Ok(out)
}

/// One hop: its address, and its IPv6 zone if it names one.
type Hop<'a> = (IpAddr, Option<&'a str>);

/// An address as Go's netip.ParseAddr reads it: IPv6 may carry a zone.
fn parse_addr(s: &str) -> Option<Hop<'_>> {
    match s.split_once('%') {
        None => s.parse().ok().map(|a| (a, None)),
        Some((addr, zone)) if !zone.is_empty() => {
            let v6: Ipv6Addr = addr.parse().ok()?;
            // unmapped, an IPv4 address has no zone
            Some(v6.to_ipv4_mapped().map_or((IpAddr::V6(v6), Some(zone)), |v4| (IpAddr::V4(v4), None)))
        }
        Some(_) => None,
    }
}

/// One X-Forwarded-For entry: an address, optionally with a port as some
/// proxies send it ("1.2.3.4:5678", "[2001:db8::1]:5678").
fn parse_hop(h: &str) -> Option<Hop<'_>> {
    let h = h.trim();
    let with_port = || {
        let (host, port) = h.rsplit_once(':')?;
        if port.is_empty() || !port.bytes().all(|b| b.is_ascii_digit()) || port.parse::<u16>().is_err() {
            return None;
        }
        match host.strip_prefix('[') {
            Some(inner) => parse_addr(inner.strip_suffix(']')?).filter(|(a, _)| a.is_ipv6()),
            None => parse_addr(host).filter(|(a, _)| a.is_ipv4()),
        }
    };
    with_port().or_else(|| parse_addr(h))
}

fn is_trusted(trusted: &[Prefix], (addr, zone): Hop) -> bool {
    // an address with a zone is in no block
    zone.is_none() && trusted.iter().any(|p| p.contains(unmap(addr)))
}

/// The client behind `peer`: X-Forwarded-For read right to left, skipping
/// trusted hops, starting from the peer. The first address that is not a
/// trusted proxy is the client; an entry that is no address ends the walk.
/// With no trusted proxies the header is never read.
pub fn client<'a>(peer: IpAddr, forwarded: impl DoubleEndedIterator<Item = &'a str>, trusted: &[Prefix]) -> String {
    let mut client: Hop = (peer, None);
    if is_trusted(trusted, client) {
        // every header line, as one comma-separated list
        for hop in forwarded.rev().flat_map(|line| line.rsplit(',')) {
            let Some(hop) = parse_hop(hop) else { break };
            client = hop;
            if !is_trusted(trusted, hop) {
                break;
            }
        }
    }
    match client {
        (addr, Some(zone)) => format!("{addr}%{zone}"),
        (addr, None) => unmap(addr).to_string(),
    }
}

/// The trusted proxies, once client addresses are recorded.
static TRUSTED: OnceLock<Vec<Prefix>> = OnceLock::new();

/// Record client addresses from now on.
pub fn enable(trusted: Vec<Prefix>) {
    let _ = TRUSTED.set(trusted);
}

/// The client address for a request's usage event; None unless enabled.
pub fn of(peer: Option<IpAddr>, headers: &HeaderMap) -> Option<Box<str>> {
    let trusted = TRUSTED.get()?;
    // a line that is not text is no address: the walk ends there
    let lines = headers.get_all("x-forwarded-for").iter().map(|v| v.to_str().unwrap_or("?"));
    Some(client(peer?, lines, trusted).into())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn seen(peer: &str, forwarded: &[&str], trusted: &str) -> String {
        client(peer.parse().unwrap(), forwarded.iter().copied(), &parse_trusted(trusted).unwrap())
    }

    // The cases of internal/console's client address tests, and the forms
    // parseHop takes.
    #[test]
    fn the_client_is_the_first_address_that_is_not_a_trusted_proxy() {
        let lb = "10.0.0.0/8, 192.168.1.1, 2001:db8:aa::/48";
        for (peer, forwarded, trusted, want) in [
            // no trusted proxies: the peer, whatever the header says
            ("203.0.113.9", &["1.2.3.4"][..], "", "203.0.113.9"),
            ("203.0.113.9", &[][..], lb, "203.0.113.9"),
            // an untrusted peer's header is never read
            ("203.0.113.9", &["1.2.3.4"], lb, "203.0.113.9"),
            // a trusted peer: the rightmost untrusted hop
            ("10.1.2.3", &["1.2.3.4"], lb, "1.2.3.4"),
            ("10.1.2.3", &["6.6.6.6, 1.2.3.4, 10.9.9.9"], lb, "1.2.3.4"),
            ("10.1.2.3", &["6.6.6.6, 1.2.3.4", "192.168.1.1"], lb, "1.2.3.4"),
            ("10.1.2.3", &["6.6.6.6", "1.2.3.4,10.0.0.1 "], lb, "1.2.3.4"),
            // every hop trusted: the leftmost
            ("10.1.2.3", &["10.0.0.7, 10.0.0.8"], lb, "10.0.0.7"),
            // a trusted peer without the header
            ("10.1.2.3", &[], lb, "10.1.2.3"),
            ("10.1.2.3", &[""], lb, "10.1.2.3"),
            // what is no address ends the walk at the last one that was
            ("10.1.2.3", &["1.2.3.4, unknown, 10.0.0.8"], lb, "10.0.0.8"),
            ("10.1.2.3", &["1.2.3.4, , 10.0.0.8"], lb, "10.0.0.8"),
            ("10.1.2.3", &["1.2.3.4, _hidden"], lb, "10.1.2.3"),
            // ports
            ("10.1.2.3", &["1.2.3.4:5678"], lb, "1.2.3.4"),
            ("10.1.2.3", &["1.2.3.4, 10.0.0.8:443"], lb, "1.2.3.4"),
            ("10.1.2.3", &["[2001:db8::1]:5678"], lb, "2001:db8::1"),
            ("10.1.2.3", &["1.2.3.4, [1.2.3.9]:80"], lb, "10.1.2.3"),
            ("10.1.2.3", &["1.2.3.4:99999"], lb, "10.1.2.3"),
            ("10.1.2.3", &["1.2.3.4:+80"], lb, "10.1.2.3"),
            // IPv6, bare: the last group is not a port
            ("10.1.2.3", &["2001:db8::1:80"], lb, "2001:db8::1:80"),
            ("2001:db8:aa::5", &["2001:db8:bb::1, 2001:db8:aa:1::2"], lb, "2001:db8:bb::1"),
            ("2001:db8:ab::5", &["1.2.3.4"], lb, "2001:db8:ab::5"),
            // IPv4 in IPv6 is the IPv4 address, for the blocks and in the answer
            ("::ffff:10.1.2.3", &["::ffff:1.2.3.4"], lb, "1.2.3.4"),
            ("::ffff:203.0.113.9", &["1.2.3.4"], lb, "203.0.113.9"),
            ("10.1.2.3", &["1.2.3.4, ::ffff:10.0.0.9"], lb, "1.2.3.4"),
            // a zone is kept, and is in no block
            ("10.1.2.3", &["1.2.3.4, fe80::1%eth0"], "10.0.0.0/8, fe80::/10", "fe80::1%eth0"),
            ("10.1.2.3", &["[fe80::1%eth0]:80"], lb, "fe80::1%eth0"),
            ("10.1.2.3", &["1.2.3.4, fe80::1%"], lb, "10.1.2.3"),
            ("10.1.2.3", &["1.2.3.4, 10.0.0.1%eth0"], lb, "10.1.2.3"),
            ("10.1.2.3", &["1.2.3.4, ::ffff:10.0.0.1%eth0"], lb, "1.2.3.4"),
            ("10.1.2.3", &["::ffff:1.2.3.4%eth0"], lb, "1.2.3.4"),
            // a block's family is its own
            ("10.1.2.3", &["1.2.3.4"], "::/0", "10.1.2.3"),
            ("2001:db8::1", &["1.2.3.4"], "0.0.0.0/0", "2001:db8::1"),
            ("2001:db8::1", &["1.2.3.4, 9.9.9.9"], "::/0, 9.0.0.0/8", "1.2.3.4"),
        ] {
            assert_eq!(seen(peer, forwarded, trusted), want, "{peer} {forwarded:?} trusting {trusted:?}");
        }
    }

    #[test]
    fn reads_the_trusted_list() {
        assert_eq!(parse_trusted("").unwrap(), []);
        assert_eq!(parse_trusted(" , ").unwrap(), []);
        let p = parse_trusted("10.1.2.3/8, 192.168.1.1 ,::ffff:10.0.0.1,2001:db8::1/32,0.0.0.0/0").unwrap();
        let block = |net: &str, bits| Prefix { net: net.parse().unwrap(), bits };
        assert_eq!(
            p,
            [
                block("10.0.0.0", 8),
                block("192.168.1.1", 32),
                block("10.0.0.1", 32),
                block("2001:db8::", 32),
                block("0.0.0.0", 0)
            ]
        );
        for bad in [
            "proxy",
            "10.0.0.0/33",
            "10.0.0.0/",
            "10.0.0.0/+8",
            "10.0.0.0/08",
            "::/129",
            "10.0.0.1%eth0",
            "1.2.3.4/8/9",
        ] {
            assert!(parse_trusted(bad).is_err(), "{bad}");
        }
    }
}
