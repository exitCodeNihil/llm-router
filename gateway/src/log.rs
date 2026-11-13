//! Leveled logging to stderr: LLMR_LOG_LEVEL (debug, info, warn, error) and
//! LLMR_LOG_FORMAT (text, json). A line is built whole and written once, and a
//! failed write is ignored: stderr may be a closed pipe.

use std::io::Write;
use std::sync::atomic::{AtomicBool, AtomicU8, Ordering::Relaxed};

#[derive(Clone, Copy, PartialEq, PartialOrd, Debug)]
pub enum Level {
    Debug,
    Info,
    Warn,
    Error,
}

impl Level {
    fn name(self) -> &'static str {
        ["DEBUG", "INFO", "WARN", "ERROR"][self as usize]
    }
}

static LEVEL: AtomicU8 = AtomicU8::new(Level::Info as u8);
static JSON: AtomicBool = AtomicBool::new(false);

/// The most bytes of a message a line carries: control-plane and upstream
/// answers end up in messages.
const MAX_MESSAGE: usize = 2000;

/// Set the level and format from the environment's values; unset keeps info
/// and text.
pub fn init(level: Option<&str>, format: Option<&str>) -> Result<(), String> {
    let level = match level {
        None | Some("info") => Level::Info,
        Some("debug") => Level::Debug,
        Some("warn") => Level::Warn,
        Some("error") => Level::Error,
        Some(v) => return Err(format!("LLMR_LOG_LEVEL={v:?}: use debug, info, warn or error")),
    };
    let json = match format {
        None | Some("text") => false,
        Some("json") => true,
        Some(v) => return Err(format!("LLMR_LOG_FORMAT={v:?}: use text or json")),
    };
    LEVEL.store(level as u8, Relaxed);
    JSON.store(json, Relaxed);
    Ok(())
}

pub fn enabled(level: Level) -> bool {
    level as u8 >= LEVEL.load(Relaxed)
}

/// Write one line; the macros call this once `enabled` says so.
pub fn write(level: Level, args: std::fmt::Arguments) {
    let line = line(JSON.load(Relaxed), crate::now_ms(), level, &args.to_string());
    let _ = std::io::stderr().write_all(line.as_bytes());
}

fn line(json: bool, ms: i64, level: Level, msg: &str) -> String {
    let mut end = msg.len().min(MAX_MESSAGE);
    while !msg.is_char_boundary(end) {
        end -= 1;
    }
    let (msg, cut) = (&msg[..end], if end < msg.len() { "…" } else { "" });
    let ts = timestamp(ms);
    if json {
        let msg = serde_json::to_string(&format!("{msg}{cut}")).unwrap_or_else(|_| "\"\"".into());
        return format!("{{\"ts\":\"{ts}\",\"level\":\"{}\",\"msg\":{msg}}}\n", level.name().to_ascii_lowercase());
    }
    let mut out = format!("{ts} {} ", level.name());
    // One message, one line: a line break in it could pass for another entry.
    for c in msg.chars() {
        match c {
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            c if c.is_control() => out.push(' '),
            c => out.push(c),
        }
    }
    out.push_str(cut);
    out.push('\n');
    out
}

/// RFC 3339 in UTC, to the millisecond.
fn timestamp(ms: i64) -> String {
    let (days, rem) = (ms.div_euclid(86_400_000), ms.rem_euclid(86_400_000));
    // Days to a civil date (Howard Hinnant's algorithm), years counted from March.
    let z = days + 719_468;
    let (era, doe) = (z.div_euclid(146_097), z.rem_euclid(146_097));
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + (month <= 2) as i64;
    let (h, m, s, milli) = (rem / 3_600_000, rem / 60_000 % 60, rem / 1000 % 60, rem % 1000);
    format!("{year:04}-{month:02}-{day:02}T{h:02}:{m:02}:{s:02}.{milli:03}Z")
}

#[macro_export]
macro_rules! log_at {
    ($level:ident, $($arg:tt)*) => {
        if $crate::log::enabled($crate::log::Level::$level) {
            $crate::log::write($crate::log::Level::$level, format_args!($($arg)*));
        }
    };
}

#[macro_export]
macro_rules! log_debug {
    ($($arg:tt)*) => { $crate::log_at!(Debug, $($arg)*) };
}

#[macro_export]
macro_rules! log_info {
    ($($arg:tt)*) => { $crate::log_at!(Info, $($arg)*) };
}

#[macro_export]
macro_rules! log_warn {
    ($($arg:tt)*) => { $crate::log_at!(Warn, $($arg)*) };
}

#[macro_export]
macro_rules! log_error {
    ($($arg:tt)*) => { $crate::log_at!(Error, $($arg)*) };
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn timestamps_are_rfc3339_utc() {
        for (ms, want) in [
            (0, "1970-01-01T00:00:00.000Z"),
            (951_782_400_000, "2000-02-29T00:00:00.000Z"),
            (1_709_251_199_999, "2024-02-29T23:59:59.999Z"),
            (1_709_251_200_000, "2024-03-01T00:00:00.000Z"),
            (1_794_562_503_045, "2026-11-13T09:35:03.045Z"),
            (4_102_444_799_000, "2099-12-31T23:59:59.000Z"),
            (-1, "1969-12-31T23:59:59.999Z"),
        ] {
            assert_eq!(timestamp(ms), want);
        }
    }

    #[test]
    fn text_lines_stay_on_one_line() {
        assert_eq!(line(false, 0, Level::Warn, "join failed"), "1970-01-01T00:00:00.000Z WARN join failed\n");
        let forged = line(false, 0, Level::Info, "a\r\n2026-01-01T00:00:00.000Z ERROR forged\u{7}");
        assert_eq!(forged, "1970-01-01T00:00:00.000Z INFO a\\r\\n2026-01-01T00:00:00.000Z ERROR forged \n");
    }

    #[test]
    fn json_lines_escape_the_message() {
        let out = line(true, 0, Level::Error, "say \"hi\"\n\u{1}\\ é");
        assert_eq!(
            out,
            "{\"ts\":\"1970-01-01T00:00:00.000Z\",\"level\":\"error\",\"msg\":\"say \\\"hi\\\"\\n\\u0001\\\\ é\"}\n"
        );
        let v: serde_json::Value = serde_json::from_str(&out).unwrap();
        assert_eq!(v["msg"], "say \"hi\"\n\u{1}\\ é");
    }

    #[test]
    fn long_messages_are_cut_on_a_char_boundary() {
        let msg = format!("{}é{}", "x".repeat(MAX_MESSAGE - 1), "y".repeat(50));
        for json in [false, true] {
            let out = line(json, 0, Level::Info, &msg);
            assert!(out.contains(&format!("{}…", "x".repeat(MAX_MESSAGE - 1))) && !out.contains('é'), "{out}");
            assert!(!out.contains('y') && out.ends_with('\n'));
        }
        assert!(!line(false, 0, Level::Info, &"x".repeat(MAX_MESSAGE)).contains('…'));
    }

    #[test]
    fn levels_and_formats_parse() {
        assert!(init(Some("verbose"), None).unwrap_err().contains("LLMR_LOG_LEVEL"));
        assert!(init(None, Some("yaml")).unwrap_err().contains("LLMR_LOG_FORMAT"));
        assert!(Level::Debug < Level::Info && Level::Warn < Level::Error);
    }
}
