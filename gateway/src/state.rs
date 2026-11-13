//! What a gateway keeps on disk (LLMR_STATE_DIR) to start without the control
//! plane: the last config as it was received, and when it was last confirmed
//! along with the spend known then; and, while the control plane refuses the
//! gateway, since when. All are sealed with a key derived from the join token,
//! so the files are useless without it.
//!
//! A file is `magic | version | kind | salt | nonce | ciphertext+tag`:
//! AES-256-GCM under HKDF-SHA256(join token, salt), the first three fields
//! authenticated with it.

use std::collections::HashMap;
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::sync::atomic::Ordering;
use std::time::Duration;

use bytes::Bytes;
use ring::rand::SecureRandom;
use ring::{aead, hkdf};
use serde::{Deserialize, Serialize};

use crate::{log_warn, metrics};

const MAGIC: &[u8; 8] = b"LLMRGWST";
const VERSION: u8 = 1;
const INFO: &[u8] = b"llmr-gw-state-v1";
const HEADER: usize = MAGIC.len() + 2;
const SALT: usize = 16;
const SEALED: usize = HEADER + SALT + aead::NONCE_LEN;

const CONFIG: (&str, u8) = ("config.bin", 1);
const CONFIRMED: (&str, u8) = ("confirmed.bin", 2);
const REFUSED: (&str, u8) = ("refused.bin", 3);

/// Files are written this far apart at most.
pub const SAVE_INTERVAL: Duration = Duration::from_secs(30);

/// When the config on disk was last confirmed, and the spend known then.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Confirmed {
    /// Wall clock of the last config poll the control plane answered 200 or 304.
    pub confirmed_at_ms: i64,
    pub version: i64,
    pub group: String,
    /// The control plane's spend per budgeted scope; None when no sync had
    /// brought it yet.
    pub spend: Option<HashMap<String, f64>>,
}

/// A config response, and whether its body is gzipped.
pub struct Saved {
    pub gzip: bool,
    pub config: Vec<u8>,
    pub confirmed: Confirmed,
}

#[derive(Default)]
struct Pending {
    config: Option<(bool, Bytes)>,
    /// The config in use; None until one arrives from the control plane.
    confirmed: Option<Confirmed>,
    dirty: bool,
}

pub struct Store {
    dir: PathBuf,
    token: String,
    /// Held for the life of the process: one gateway per directory.
    _lock: File,
    /// Held across file writes and deletes, so a delete is never undone by a
    /// write that began before it.
    io: Mutex<()>,
    pending: Mutex<Pending>,
    changed: tokio::sync::Notify,
}

impl Store {
    /// Take the directory for this process. An Err means running without
    /// saved state: the directory is unusable, or another process holds it.
    pub fn open(dir: &Path, token: &str) -> Result<Store, String> {
        metrics::STATE_DIR_LOCKED.store(0, Ordering::Relaxed);
        fs::create_dir_all(dir).map_err(|e| format!("{}: {e}", dir.display()))?;
        let path = dir.join("lock");
        let lock = OpenOptions::new()
            .create(true)
            .truncate(false)
            .write(true)
            .mode(0o600)
            .open(&path)
            .map_err(|e| format!("{}: {e}", path.display()))?;
        lock.try_lock().map_err(|e| match e {
            fs::TryLockError::WouldBlock => format!("{} is in use by another process", dir.display()),
            fs::TryLockError::Error(e) => format!("{}: {e}", path.display()),
        })?;
        metrics::STATE_DIR_LOCKED.store(1, Ordering::Relaxed);
        Ok(Store {
            dir: dir.to_path_buf(),
            token: token.to_string(),
            _lock: lock,
            io: Default::default(),
            pending: Default::default(),
            changed: Default::default(),
        })
    }

    /// A config the control plane just sent, to save with the next write.
    pub fn put_config(&self, gzip: bool, body: Bytes, confirmed_at_ms: i64, version: i64, group: &str) {
        let mut p = self.pending.lock().unwrap();
        p.config = Some((gzip, body));
        p.confirmed = Some(Confirmed { confirmed_at_ms, version, group: group.to_string(), spend: None });
        p.dirty = true;
        self.changed.notify_one();
    }

    /// The control plane confirmed the config in use.
    pub fn confirm(&self, at_ms: i64) {
        let mut p = self.pending.lock().unwrap();
        if let Some(c) = &mut p.confirmed {
            c.confirmed_at_ms = at_ms;
            p.dirty = true;
            self.changed.notify_one();
        }
    }

    /// Spend changed: save it with the next write.
    pub fn touch(&self) {
        let mut p = self.pending.lock().unwrap();
        if p.confirmed.is_some() {
            p.dirty = true;
            self.changed.notify_one();
        }
    }

    /// Resolves once there is something to write.
    pub async fn changed(&self) {
        self.changed.notified().await
    }

    /// Write what changed since the last call; `spend` supplies the figures
    /// to go with it. Blocking.
    pub fn flush(&self, spend: impl FnOnce() -> Option<HashMap<String, f64>>) {
        let _io = self.io.lock().unwrap();
        let (config, confirmed) = {
            let mut p = self.pending.lock().unwrap();
            let confirmed = if std::mem::take(&mut p.dirty) { p.confirmed.clone() } else { None };
            (p.config.take(), confirmed)
        };
        // The config first: confirmed.bin must never name a version newer
        // than the one beside it.
        if let Some((gzip, body)) = config {
            let mut plain = Vec::with_capacity(body.len() + 1);
            plain.push(gzip as u8);
            plain.extend_from_slice(&body);
            if let Err(e) = self.write(CONFIG, plain) {
                log_warn!("state: saving the config: {e}");
                return;
            }
        }
        if let Some(mut c) = confirmed {
            c.spend = spend();
            if let Err(e) = self.write(CONFIRMED, serde_json::to_vec(&c).unwrap_or_default()) {
                log_warn!("state: saving the spend: {e}");
            }
        }
    }

    fn write(&self, (name, kind): (&str, u8), plain: Vec<u8>) -> Result<(), String> {
        let sealed = seal(self.token.as_bytes(), kind, plain)?;
        let (tmp, path) = (self.dir.join(format!("{name}.tmp")), self.dir.join(name));
        (|| {
            let mut f = OpenOptions::new().create(true).truncate(true).write(true).mode(0o600).open(&tmp)?;
            f.write_all(&sealed)?;
            f.sync_all()?;
            fs::rename(&tmp, &path)?;
            // the rename itself is durable once the directory is synced
            File::open(&self.dir)?.sync_all()
        })()
        .map_err(|e| format!("{}: {e}", path.display()))
    }

    /// Read both files. A file that cannot be read, opened with this join
    /// token or parsed is deleted; None unless both are good. Blocking.
    pub fn load(&self) -> Option<Saved> {
        let _io = self.io.lock().unwrap();
        let config = self.read(CONFIG);
        let confirmed = self.read(CONFIRMED).and_then(|plain| {
            serde_json::from_slice::<Confirmed>(&plain).map_err(|e| self.unusable(CONFIRMED.0, &e.to_string())).ok()
        });
        let (mut config, confirmed) = (config?, confirmed?);
        if config.is_empty() || config[0] > 1 {
            self.unusable(CONFIG.0, "malformed");
            return None;
        }
        let gzip = config.remove(0) == 1;
        Some(Saved { gzip, config, confirmed })
    }

    fn read(&self, (name, kind): (&str, u8)) -> Option<Vec<u8>> {
        let sealed = match fs::read(self.dir.join(name)) {
            Ok(b) => b,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return None,
            Err(e) => {
                self.unusable(name, &e.to_string());
                return None;
            }
        };
        open(self.token.as_bytes(), kind, &sealed).map_err(|e| self.unusable(name, &e)).ok()
    }

    fn unusable(&self, name: &str, why: &str) {
        log_warn!("state: {name} is unusable ({why}); deleting it");
        let _ = fs::remove_file(self.dir.join(name));
    }

    /// The control plane began refusing this gateway's join at `at_ms`:
    /// kept until a join succeeds, so a restart does not start the count
    /// again. Blocking.
    pub fn put_refused(&self, at_ms: i64) {
        let _io = self.io.lock().unwrap();
        if let Err(e) = self.write(REFUSED, at_ms.to_le_bytes().to_vec()) {
            log_warn!("state: saving the refusal: {e}");
        }
    }

    /// Since when the control plane has refused this gateway, if it does.
    /// Blocking.
    pub fn refused_at(&self) -> Option<i64> {
        let _io = self.io.lock().unwrap();
        let plain = self.read(REFUSED)?;
        match <[u8; 8]>::try_from(&plain[..]) {
            Ok(at) => Some(i64::from_le_bytes(at)),
            Err(_) => {
                self.unusable(REFUSED.0, "malformed");
                None
            }
        }
    }

    /// A join succeeded. Blocking.
    pub fn clear_refused(&self) {
        let _io = self.io.lock().unwrap();
        let _ = fs::remove_file(self.dir.join(REFUSED.0));
    }

    /// Delete the saved config and spend and drop what waits to be written;
    /// a refusal on record stays. Blocking.
    pub fn forget(&self) {
        let _io = self.io.lock().unwrap();
        *self.pending.lock().unwrap() = Pending::default();
        for name in [CONFIG.0, CONFIRMED.0] {
            let _ = fs::remove_file(self.dir.join(name));
            let _ = fs::remove_file(self.dir.join(format!("{name}.tmp")));
        }
    }
}

fn key(token: &[u8], salt: &[u8]) -> Result<aead::LessSafeKey, String> {
    let prk = hkdf::Salt::new(hkdf::HKDF_SHA256, salt).extract(token);
    let okm = prk.expand(&[INFO], &aead::AES_256_GCM).map_err(|_| "key derivation failed".to_string())?;
    Ok(aead::LessSafeKey::new(okm.into()))
}

fn header(kind: u8) -> [u8; HEADER] {
    let mut h = [0; HEADER];
    h[..MAGIC.len()].copy_from_slice(MAGIC);
    h[MAGIC.len()] = VERSION;
    h[MAGIC.len() + 1] = kind;
    h
}

fn seal(token: &[u8], kind: u8, plain: Vec<u8>) -> Result<Vec<u8>, String> {
    let mut random = [0u8; SALT + aead::NONCE_LEN];
    ring::rand::SystemRandom::new().fill(&mut random).map_err(|_| "no randomness".to_string())?;
    let (salt, nonce) = random.split_at(SALT);
    let head = header(kind);
    let mut out = Vec::with_capacity(SEALED + plain.len() + aead::MAX_TAG_LEN);
    out.extend_from_slice(&head);
    out.extend_from_slice(&random);
    out.extend_from_slice(&plain);
    let nonce = aead::Nonce::try_assume_unique_for_key(nonce).map_err(|_| "bad nonce".to_string())?;
    let tag = key(token, salt)?
        .seal_in_place_separate_tag(nonce, aead::Aad::from(head), &mut out[SEALED..])
        .map_err(|_| "sealing failed".to_string())?;
    out.extend_from_slice(tag.as_ref());
    Ok(out)
}

fn open(token: &[u8], kind: u8, sealed: &[u8]) -> Result<Vec<u8>, String> {
    let head = header(kind);
    if sealed.len() < SEALED + aead::MAX_TAG_LEN || sealed[..HEADER] != head {
        return Err("not a state file of this version".into());
    }
    let (salt, nonce) = sealed[HEADER..SEALED].split_at(SALT);
    let nonce = aead::Nonce::try_assume_unique_for_key(nonce).map_err(|_| "bad nonce".to_string())?;
    let mut body = sealed[SEALED..].to_vec();
    let n = key(token, salt)?
        .open_in_place(nonce, aead::Aad::from(head), &mut body)
        .map_err(|_| "wrong join token, or the file was altered".to_string())?
        .len();
    body.truncate(n);
    Ok(body)
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    /// A fresh directory under the system's temporary one, removed on drop.
    pub(crate) struct TempDir(pub PathBuf);

    impl TempDir {
        pub(crate) fn new(what: &str) -> TempDir {
            let dir =
                std::env::temp_dir().join(format!("llmr-gw-{what}-{}-{:016x}", std::process::id(), fastrand::u64(..)));
            fs::create_dir_all(&dir).unwrap();
            TempDir(dir)
        }
    }

    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn save(store: &Store, version: i64, spend: Option<HashMap<String, f64>>) {
        store.put_config(true, Bytes::from_static(b"\x1f\x8bconfig"), 1_700_000_000_000, version, "g1");
        store.flush(|| spend);
    }

    #[test]
    fn sealed_bytes_open_only_as_what_they_were_sealed_as() {
        let sealed = seal(b"llmrg_a", 1, b"provider credentials".to_vec()).unwrap();
        assert_eq!(open(b"llmrg_a", 1, &sealed).unwrap(), b"provider credentials");
        assert!(!sealed.windows(8).any(|w| w == b"provider"), "the content is not readable");
        assert_ne!(sealed, seal(b"llmrg_a", 1, b"provider credentials".to_vec()).unwrap(), "fresh salt and nonce");
        assert!(open(b"llmrg_b", 1, &sealed).is_err(), "another join token");
        assert!(open(b"llmrg_a", 2, &sealed).is_err(), "another kind of file");
        assert!(open(b"llmrg_a", 1, &sealed[..sealed.len() - 1]).is_err(), "cut short");
        assert!(open(b"llmrg_a", 1, &sealed[..SEALED]).is_err());
        for i in 0..sealed.len() {
            let mut bad = sealed.clone();
            bad[i] ^= 1;
            assert!(open(b"llmrg_a", 1, &bad).is_err(), "byte {i} altered");
        }
        assert_eq!(open(b"llmrg_a", 2, &seal(b"llmrg_a", 2, Vec::new()).unwrap()).unwrap(), b"");
    }

    #[test]
    fn what_is_saved_comes_back() {
        let dir = TempDir::new("roundtrip");
        let spend = HashMap::from([("key:k".to_string(), 1.25)]);
        {
            let store = Store::open(&dir.0, "llmrg_a").unwrap();
            assert!(store.load().is_none(), "nothing saved yet");
            save(&store, 7, Some(spend.clone()));
            assert_eq!(store.load().unwrap().confirmed.spend, Some(spend.clone()));
            store.touch();
            store.flush(|| Some(HashMap::new()));
            assert_eq!(store.load().unwrap().confirmed.spend, Some(HashMap::new()));
            store.confirm(1_700_000_050_000);
            store.flush(|| None);
            use std::os::unix::fs::PermissionsExt;
            for name in ["config.bin", "confirmed.bin"] {
                assert_eq!(fs::metadata(dir.0.join(name)).unwrap().permissions().mode() & 0o777, 0o600);
                assert!(!dir.0.join(format!("{name}.tmp")).exists());
            }
        }
        let store = Store::open(&dir.0, "llmrg_a").unwrap();
        let saved = store.load().unwrap();
        assert!(saved.gzip);
        assert_eq!(saved.config, b"\x1f\x8bconfig");
        let want = Confirmed { confirmed_at_ms: 1_700_000_050_000, version: 7, group: "g1".into(), spend: None };
        assert_eq!(saved.confirmed, want, "the later write stands, with the spend known then");
    }

    #[test]
    fn nothing_is_written_before_a_config_arrives() {
        let dir = TempDir::new("idle");
        let store = Store::open(&dir.0, "llmrg_a").unwrap();
        store.confirm(1);
        store.touch();
        store.flush(|| Some(HashMap::new()));
        assert!(!dir.0.join("confirmed.bin").exists() && !dir.0.join("config.bin").exists());
    }

    #[test]
    fn another_join_token_gets_nothing_and_the_files_are_removed() {
        let dir = TempDir::new("rotated");
        save(&Store::open(&dir.0, "llmrg_old").unwrap(), 1, None);
        let store = Store::open(&dir.0, "llmrg_new").unwrap();
        assert!(store.load().is_none());
        assert!(!dir.0.join("config.bin").exists() && !dir.0.join("confirmed.bin").exists());
    }

    #[test]
    fn an_altered_file_is_refused_and_removed() {
        for name in ["config.bin", "confirmed.bin"] {
            let dir = TempDir::new("altered");
            let store = Store::open(&dir.0, "llmrg_a").unwrap();
            save(&store, 1, None);
            let path = dir.0.join(name);
            let mut bytes = fs::read(&path).unwrap();
            let last = bytes.len() - 1;
            bytes[last] ^= 0x80;
            fs::write(&path, bytes).unwrap();
            assert!(store.load().is_none(), "{name}");
            assert!(!path.exists(), "{name} is deleted");
        }
    }

    #[test]
    fn a_refusal_stays_on_record_until_cleared() {
        let dir = TempDir::new("refusal");
        let store = Store::open(&dir.0, "llmrg_a").unwrap();
        assert_eq!(store.refused_at(), None);
        store.put_refused(1_700_000_000_123);
        save(&store, 1, None);
        store.forget();
        drop(store);
        let store = Store::open(&dir.0, "llmrg_a").unwrap();
        assert_eq!(store.refused_at(), Some(1_700_000_000_123), "across a restart, and after the config is forgotten");
        store.clear_refused();
        assert_eq!(store.refused_at(), None);
        assert!(!dir.0.join("refused.bin").exists());

        // one written under another join token says nothing about this one
        store.put_refused(1);
        drop(store);
        let store = Store::open(&dir.0, "llmrg_b").unwrap();
        assert_eq!(store.refused_at(), None);
        assert!(!dir.0.join("refused.bin").exists());
    }

    #[test]
    fn a_directory_held_by_another_process_is_left_alone() {
        let dir = TempDir::new("locked");
        let first = Store::open(&dir.0, "llmrg_a").unwrap();
        let err = Store::open(&dir.0, "llmrg_a").err().unwrap();
        assert!(err.contains("in use by another process"), "{err}");
        drop(first);
        assert!(Store::open(&dir.0, "llmrg_a").is_ok(), "free again once its holder is gone");
    }

    #[test]
    fn forgetting_deletes_the_files_and_what_was_waiting() {
        let dir = TempDir::new("forget");
        let store = Store::open(&dir.0, "llmrg_a").unwrap();
        save(&store, 1, None);
        store.put_config(false, Bytes::from_static(b"{}"), 1, 2, "g1");
        store.forget();
        store.flush(|| None);
        assert!(!dir.0.join("config.bin").exists() && !dir.0.join("confirmed.bin").exists());
        assert!(store.load().is_none());
    }
}
