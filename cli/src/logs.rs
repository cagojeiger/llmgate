use std::{
    fs::{self, File, OpenOptions},
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
        Self { sender, dropped }
    }
    pub fn message(&self, data: &[u8]) {
        if self.sender.try_send(data.to_vec()).is_err() {
            self.dropped.fetch_add(1, Ordering::Relaxed);
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
    let _ = fs::remove_file(path.with_extension("log.4"));
    for n in (1..4).rev() {
        let _ = fs::rename(
            path.with_extension(format!("log.{n}")),
            path.with_extension(format!("log.{}", n + 1)),
        );
    }
    if path.exists() {
        fs::rename(path, path.with_extension("log.1"))?;
    }
    Ok(())
}

pub fn supervisor_output(path: PathBuf) -> std::io::Result<File> {
    // Only startup/final errors use this fd; rotate every run instead of accumulating forever.
    rotate(&path)?;
    use std::os::unix::fs::OpenOptionsExt;
    OpenOptions::new()
        .create(true)
        .append(true)
        .mode(0o600)
        .open(path)
}
