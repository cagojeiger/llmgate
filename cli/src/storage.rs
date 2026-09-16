use crate::profile::Profile;
use anyhow::{Context, ensure};
use fs2::FileExt;
use serde::{Serialize, de::DeserializeOwned};
use std::{
    fs::{self, File, OpenOptions},
    os::unix::fs::{OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
};

#[derive(Clone)]
pub struct Home(pub PathBuf);
impl Home {
    pub fn open(path: Option<PathBuf>) -> anyhow::Result<Self> {
        let path = path.unwrap_or_else(|| {
            PathBuf::from(std::env::var_os("HOME").unwrap_or_default()).join(".llmgate")
        });
        ensure!(
            path.is_absolute() && path != Path::new("/"),
            "home must be an absolute dedicated directory"
        );
        fs::create_dir_all(&path)?;
        let path = path.canonicalize()?;
        fs::set_permissions(&path, fs::Permissions::from_mode(0o700))?;
        for dir in [
            "tools",
            "runtimes",
            "cache/packages",
            "cache/models",
            "locks",
            "control",
            "logs",
        ] {
            fs::create_dir_all(path.join(dir))?;
        }
        ensure!(
            Profile::ALL.iter().all(|p| path
                .join(format!("control/{}.sock", p.id()))
                .as_os_str()
                .len()
                < 104),
            "home path too long for a macOS control socket"
        );
        let home = Self(path);
        if !home.0.join("identity.json").exists() {
            home.write("identity.json", &uuid::Uuid::new_v4().to_string())?;
        }
        Ok(home)
    }
    pub fn runtime(&self, p: Profile) -> PathBuf {
        self.0.join("runtimes").join(p.id())
    }
    pub fn socket(&self, p: Profile) -> PathBuf {
        self.0.join("control").join(format!("{}.sock", p.id()))
    }
    pub fn read<T: DeserializeOwned>(&self, name: &str) -> anyhow::Result<T> {
        Ok(serde_json::from_slice(&fs::read(self.0.join(name))?)?)
    }
    pub fn write<T: Serialize>(&self, name: &str, value: &T) -> anyhow::Result<()> {
        atomic_write(&self.0.join(name), &serde_json::to_vec_pretty(value)?)
    }
    pub fn profile_lock(&self, p: Profile) -> anyhow::Result<File> {
        lock(&self.0.join(format!("locks/{}.lock", p.id())), false)
    }
    pub fn engine_lock(&self, p: Profile) -> anyhow::Result<File> {
        lock(&self.0.join(format!("locks/{}-engine.lock", p.id())), false)
    }
    pub fn cache_lock(&self, exclusive: bool) -> anyhow::Result<File> {
        lock(&self.0.join("locks/cache.lock"), !exclusive)
    }
}

pub fn lock(path: &Path, shared: bool) -> anyhow::Result<File> {
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(path)?;
    if shared {
        FileExt::try_lock_shared(&file)
    } else {
        FileExt::try_lock_exclusive(&file)
    }
    .context("resource in use; stop the owning profile before cleaning")?;
    Ok(file)
}
pub fn atomic_write(path: &Path, data: &[u8]) -> anyhow::Result<()> {
    use std::io::Write;
    let tmp = path.with_extension(format!("tmp-{}", uuid::Uuid::new_v4()));
    let mut f = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&tmp)?;
    f.write_all(data)?;
    f.sync_all()?;
    fs::rename(tmp, path)?;
    Ok(())
}
pub fn bytes(path: &Path) -> anyhow::Result<u64> {
    let mut total = 0;
    for entry in fs::read_dir(path)? {
        let p = entry?.path();
        let m = fs::symlink_metadata(&p)?;
        total += if m.is_dir() { bytes(&p)? } else { m.len() };
    }
    Ok(total)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn active_runtime_prevents_cache_deletion() -> anyhow::Result<()> {
        let d = tempfile::tempdir()?;
        let h = Home::open(Some(d.path().to_owned()))?;
        let running = h.cache_lock(false)?;
        assert!(h.cache_lock(true).is_err());
        drop(running);
        assert!(h.cache_lock(true).is_ok());
        Ok(())
    }
    #[test]
    fn profile_lock_prevents_duplicate_start() -> anyhow::Result<()> {
        let d = tempfile::tempdir()?;
        let h = Home::open(Some(d.path().to_owned()))?;
        let _lock = h.profile_lock(Profile::Embedding)?;
        assert!(h.profile_lock(Profile::Embedding).is_err());
        assert!(h.profile_lock(Profile::Stt).is_ok());
        Ok(())
    }
}
