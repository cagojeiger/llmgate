use std::{
    fs::{self, OpenOptions},
    io::Write,
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
        mpsc,
    },
};
use tokio::io::{AsyncRead, AsyncReadExt};

#[derive(Clone)]
pub struct Logger {
    sender: mpsc::SyncSender<Vec<u8>>,
    pub dropped: Arc<AtomicU64>,
    last_event: Arc<AtomicU64>,
}
impl Logger {
    pub fn start(path: PathBuf) -> Self {
        let (sender, receiver) = mpsc::sync_channel::<Vec<u8>>(128);
        let dropped = Arc::new(AtomicU64::new(0));
        let counter = dropped.clone();
        std::thread::spawn(move || {
            for data in receiver {
                if write_rotated(&path, &data).is_err() {
                    counter.fetch_add(1, Ordering::Relaxed);
                }
            }
        });
        Self {
            sender,
            dropped,
            last_event: Arc::new(AtomicU64::new(0)),
        }
    }
    pub fn message(&self, data: &[u8]) {
        if self.sender.try_send(data.to_vec()).is_err() {
            self.dropped.fetch_add(1, Ordering::Relaxed);
        }
    }
    // Connection failures can arrive in bursts. Keep at most one event per second.
    pub fn event(&self, event: &'static str) {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_secs();
        if self.last_event.swap(now, Ordering::Relaxed) != now {
            self.message(format!("{now} {event}\n").as_bytes());
        }
    }
    pub fn drain<R: AsyncRead + Unpin + Send + 'static>(&self, mut reader: R) {
        let log = self.clone();
        tokio::spawn(async move {
            let mut buf = [0; 8192];
            while let Ok(n) = reader.read(&mut buf).await {
                if n == 0 {
                    break;
                }
                log.message(&buf[..n]);
            }
        });
    }
}
fn write_rotated(path: &PathBuf, data: &[u8]) -> std::io::Result<()> {
    if fs::metadata(path).map(|m| m.len()).unwrap_or(0) + data.len() as u64 > 10 * 1024 * 1024 {
        rotate(path)?;
    }
    use std::os::unix::fs::OpenOptionsExt;
    OpenOptions::new()
        .create(true)
        .append(true)
        .mode(0o600)
        .open(path)?
        .write_all(data)
}

fn rotate(path: &PathBuf) -> std::io::Result<()> {
    ignore_missing(fs::remove_file(path.with_extension("log.4")))?;
    for n in (1..4).rev() {
        ignore_missing(fs::rename(
            path.with_extension(format!("log.{n}")),
            path.with_extension(format!("log.{}", n + 1)),
        ))?;
    }
    if path.exists() {
        fs::rename(path, path.with_extension("log.1"))?;
    }
    Ok(())
}

// Synchronous lifecycle events survive normal supervisor exit without a drain race.
pub fn supervisor_event(path: PathBuf, event: &str) -> std::io::Result<()> {
    let timestamp = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    write_rotated(&path, format!("{timestamp} {event}\n").as_bytes())
}

fn ignore_missing(result: std::io::Result<()>) -> std::io::Result<()> {
    match result {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        other => other,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn failed_write_is_counted_without_blocking_sender() {
        let root = tempfile::tempdir().unwrap();
        let logger = Logger::start(root.path().to_path_buf());
        logger.message(b"event");
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(2);
        while logger.dropped.load(Ordering::Relaxed) == 0 {
            assert!(std::time::Instant::now() < deadline);
            std::thread::sleep(std::time::Duration::from_millis(5));
        }
        assert_eq!(logger.dropped.load(Ordering::Relaxed), 1);
    }

    #[test]
    fn rotation_bounds_both_log_types() {
        let root = tempfile::tempdir().unwrap();
        for name in ["stt.log", "stt-supervisor.log"] {
            let path = root.path().join(name);
            for _ in 0..7 {
                write_rotated(&path, &vec![b'x'; 10 * 1024 * 1024]).unwrap();
            }
            assert!(path.with_extension("log.4").exists());
            assert!(!path.with_extension("log.5").exists());
            assert_eq!(fs::metadata(path).unwrap().len(), 10 * 1024 * 1024);
        }
    }
}
