//! Usage events on disk until the control plane has them, so a gateway that
//! dies with events queued can hand them over when it starts again
//! (`<LLMR_STATE_DIR>/spool`).
//!
//! Each sync seals the events queued since the last one into a file named
//! `<first>-<last>.bin` after their sequence numbers; once a batch is settled,
//! files that end at or before its last event are deleted. A file is a magic
//! line, then per event: length (u32 LE), CRC-32 of the JSON (u32 LE), and the
//! event's sync JSON without captured content. Files are not encrypted: they
//! hold ids, model names, token counts and costs, never prompts, outputs or
//! credentials.

use std::collections::BTreeMap;
use std::fs::{self, File};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::Ordering;
use std::sync::{Arc, Mutex};

use crate::{log_warn, metrics, usage};

const MAGIC: &[u8; 8] = b"LLMRSPL1";
/// Events encoded per hold of the usage queue's lock.
const CHUNK: usize = 2000;
/// No event without content is near this long; a longer record is damage.
const MAX_RECORD: usize = 1 << 20;

pub struct Spool {
    dir: PathBuf,
    max_bytes: u64,
    inner: Mutex<Inner>,
}

#[derive(Default)]
struct Inner {
    /// This process's files by their last sequence number, with their size.
    files: BTreeMap<u64, (PathBuf, u64)>,
    /// Files an earlier process left, oldest first, with their size.
    leftovers: Vec<(PathBuf, u64)>,
    bytes: u64,
    /// Every queued event up to this number is on disk, or was skipped.
    sealed: u64,
}

impl Inner {
    fn count(&self) {
        metrics::SPOOL_BYTES.store(self.bytes as i64, Ordering::Relaxed);
    }
}

fn span_of(path: &Path) -> Option<(u64, u64)> {
    let (first, last) = path.file_name()?.to_str()?.strip_suffix(".bin")?.split_once('-')?;
    Some((first.parse().ok()?, last.parse().ok()?))
}

impl Spool {
    /// Take over `dir`: what an earlier process left there waits to be
    /// replayed, and this process numbers its events above it.
    pub fn open(dir: &Path, max_bytes: u64) -> Result<Spool, String> {
        fs::create_dir_all(dir).map_err(|e| format!("{}: {e}", dir.display()))?;
        let mut found = Vec::new();
        for entry in fs::read_dir(dir).map_err(|e| format!("{}: {e}", dir.display()))?.flatten() {
            let path = entry.path();
            match span_of(&path) {
                Some(span) => found.push((span, entry.metadata().map_or(0, |m| m.len()), path)),
                // a write that never finished
                None => drop(fs::remove_file(&path)),
            }
        }
        found.sort();
        let mut inner = Inner::default();
        for ((_, last), size, path) in found {
            inner.sealed = inner.sealed.max(last);
            inner.bytes += size;
            inner.leftovers.push((path, size));
        }
        usage::number_above(inner.sealed);
        inner.count();
        Ok(Spool { dir: dir.to_path_buf(), max_bytes, inner: Mutex::new(inner) })
    }

    /// Write the events queued since the last call to one file. When the
    /// spool is full they are counted and left to memory alone.
    pub async fn seal(self: &Arc<Self>) {
        let mut after = self.inner.lock().unwrap().sealed;
        let mut records = MAGIC.to_vec();
        let mut span: Option<(u64, u64, usize)> = None;
        // The queue's lock is held a chunk at a time: requests queue events
        // in between.
        while let Some((first, last, n)) = usage::encode_after(after, CHUNK, |json| frame(&mut records, json)) {
            span = Some((span.map_or(first, |s| s.0), last, span.map_or(0, |s| s.2) + n));
            after = last;
            if n < CHUNK {
                break;
            }
        }
        let Some((first, last, n)) = span else { return };
        let size = records.len() as u64;
        let full = self.inner.lock().unwrap().bytes + size > self.max_bytes;
        let written = if full {
            false
        } else {
            let (tmp, path) = (self.dir.join(format!("{first}-{last}.tmp")), self.path(first, last));
            let dir = self.dir.clone();
            let io = tokio::task::spawn_blocking(move || {
                let mut f = fs::OpenOptions::new().create(true).truncate(true).write(true).mode(0o600).open(&tmp)?;
                f.write_all(&records)?;
                f.sync_data()?;
                fs::rename(&tmp, &path)?;
                File::open(&dir)?.sync_all()
            })
            .await;
            match io {
                Ok(Ok(())) => true,
                Ok(Err(e)) => {
                    log_warn!("spool: writing {n} events: {e}");
                    false
                }
                Err(_) => false,
            }
        };
        let mut inner = self.inner.lock().unwrap();
        inner.sealed = last;
        if written {
            inner.files.insert(last, (self.path(first, last), size));
            inner.bytes += size;
            inner.count();
        } else {
            metrics::SPOOL_SKIPPED_EVENTS.fetch_add(n as u64, Ordering::Relaxed);
        }
    }

    fn path(&self, first: u64, last: u64) -> PathBuf {
        // padded, so names sort in the order of their events
        self.dir.join(format!("{first:020}-{last:020}.bin"))
    }

    /// Every event numbered up to `seq` is settled: acknowledged by the
    /// control plane or dropped for good. Delete the files that end there.
    pub async fn settle(&self, seq: u64) {
        let done: Vec<PathBuf> = {
            let mut inner = self.inner.lock().unwrap();
            let later = inner.files.split_off(&(seq.saturating_add(1)));
            let done = std::mem::replace(&mut inner.files, later);
            inner.bytes -= done.values().map(|(_, size)| size).sum::<u64>();
            inner.count();
            done.into_values().map(|(path, _)| path).collect()
        };
        if !done.is_empty() {
            let _ = tokio::task::spawn_blocking(move || done.iter().for_each(|p| drop(fs::remove_file(p)))).await;
        }
    }

    /// The files an earlier process left, oldest first.
    pub fn leftovers(&self) -> Vec<PathBuf> {
        self.inner.lock().unwrap().leftovers.iter().map(|(path, _)| path.clone()).collect()
    }

    /// The control plane has these leftover files' events, or refused them
    /// for good.
    pub async fn replayed(&self, paths: Vec<PathBuf>) {
        {
            let mut inner = self.inner.lock().unwrap();
            let before = inner.leftovers.len();
            let mut freed = 0;
            inner.leftovers.retain(|(path, size)| {
                let done = paths.contains(path);
                freed += if done { *size } else { 0 };
                !done
            });
            debug_assert!(before - inner.leftovers.len() <= paths.len());
            inner.bytes -= freed;
            inner.count();
        }
        let _ = tokio::task::spawn_blocking(move || paths.iter().for_each(|p| drop(fs::remove_file(p)))).await;
    }
}

fn frame(out: &mut Vec<u8>, json: &[u8]) {
    let mut crc = flate2::Crc::new();
    crc.update(json);
    out.extend_from_slice(&(json.len() as u32).to_le_bytes());
    out.extend_from_slice(&crc.sum().to_le_bytes());
    out.extend_from_slice(json);
}

/// The events in a spool file, each as its JSON, and how many bytes after
/// them were not usable: a record cut short by a crash, or damaged. Reading
/// stops at the first such record. Blocking.
pub fn read(path: &Path) -> (Vec<Vec<u8>>, usize) {
    let bytes = fs::read(path).unwrap_or_default();
    let Some(mut rest) = bytes.strip_prefix(&MAGIC[..]) else { return (Vec::new(), bytes.len()) };
    let mut events = Vec::new();
    while rest.len() >= 8 {
        let len = u32::from_le_bytes([rest[0], rest[1], rest[2], rest[3]]) as usize;
        let sum = u32::from_le_bytes([rest[4], rest[5], rest[6], rest[7]]);
        if len == 0 || len > MAX_RECORD || rest.len() < 8 + len {
            break;
        }
        let json = &rest[8..8 + len];
        let mut crc = flate2::Crc::new();
        crc.update(json);
        if crc.sum() != sum {
            break;
        }
        events.push(json.to_vec());
        rest = &rest[8 + len..];
    }
    (events, rest.len())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::tests::TempDir;

    #[test]
    fn a_torn_file_gives_its_valid_prefix() {
        let dir = TempDir::new("spool-read");
        let path = dir.0.join("1-3.bin");
        let mut whole = MAGIC.to_vec();
        for json in [&br#"{"request_id":"a"}"#[..], br#"{"request_id":"b"}"#, br#"{"request_id":"c"}"#] {
            frame(&mut whole, json);
        }
        fs::write(&path, &whole).unwrap();
        let (events, torn) = read(&path);
        assert_eq!((events.len(), torn), (3, 0));
        assert_eq!(events[1], br#"{"request_id":"b"}"#);

        // cut anywhere in the last record: the first two stand
        let third = whole.len() - (8 + 18);
        for cut in [whole.len() - 1, third + 9, third + 8, third + 3] {
            fs::write(&path, &whole[..cut]).unwrap();
            let (events, torn) = read(&path);
            assert_eq!((events.len(), torn), (2, cut - third), "cut at {cut}");
        }
        // a flipped bit in the second record: only the first stands
        let mut bad = whole.clone();
        let second = MAGIC.len() + 8 + 18;
        bad[second + 12] ^= 1;
        fs::write(&path, &bad).unwrap();
        assert_eq!(read(&path).0, [br#"{"request_id":"a"}"#.to_vec()]);
        // a length that cannot be right
        let mut bad = whole.clone();
        bad[second..second + 4].copy_from_slice(&u32::MAX.to_le_bytes());
        fs::write(&path, &bad).unwrap();
        assert_eq!(read(&path).0.len(), 1);
        // not a spool file, an empty one, a missing one
        fs::write(&path, b"something else entirely").unwrap();
        assert_eq!(read(&path), (Vec::new(), 23));
        fs::write(&path, MAGIC).unwrap();
        assert_eq!(read(&path), (Vec::new(), 0));
        assert_eq!(read(&dir.0.join("9-9.bin")), (Vec::new(), 0));
    }

    #[test]
    fn file_names_carry_their_span() {
        assert_eq!(span_of(Path::new("/s/00000000000000000007-00000000000000000042.bin")), Some((7, 42)));
        assert_eq!(span_of(Path::new("/s/7-42.tmp")), None);
        assert_eq!(span_of(Path::new("/s/lock")), None);
    }
}
