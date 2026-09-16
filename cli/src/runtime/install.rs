use crate::{
    profile::Profile,
    storage::{Home, atomic_write, lock},
};
use anyhow::{Context, ensure};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{fs, path::PathBuf, process::Stdio};
use tokio::process::Command;

const UV_VERSION: &str = "0.12.15";
const UV_SHA: &str = "dc304b9ed1b24174572290fba60ac3f6fe63c73a671f0439e62a91375841964d";
const PYTHON: &str = "3.12.12";

#[derive(Serialize, Deserialize)]
pub struct Installed {
    pub python: PathBuf,
    pub model: PathBuf,
    pub fingerprint: String,
}

pub fn environment(cmd: &mut Command, home: &Home, p: Profile) {
    let rt = home.runtime(p);
    cmd.env_clear()
        .env("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
        .env("LLMGATE_MANAGED_ROOT", &home.0)
        .env("LLMGATE_PROFILE", p.id())
        .env("UV_CACHE_DIR", home.0.join("cache/packages"))
        .env("UV_PYTHON_INSTALL_DIR", rt.join("python"))
        .env("UV_PYTHON_BIN_DIR", rt.join("bin"))
        .env("UV_NO_CONFIG", "1")
        .env("UV_NO_PROJECT", "1")
        .env("HF_HOME", home.0.join("cache/models"))
        .env("HF_HUB_CACHE", home.0.join("cache/models/hub"))
        .env("HF_XET_CACHE", home.0.join("cache/models/xet"))
        .env("HF_HUB_DISABLE_TELEMETRY", "1")
        .env("XDG_CACHE_HOME", home.0.join("cache/packages/app"))
        .env("MPLCONFIGDIR", rt.join("tmp/matplotlib"))
        .env("TORCH_HOME", rt.join("tmp/torch"))
        .env("NUMBA_CACHE_DIR", rt.join("tmp/numba"))
        .env("DO_NOT_TRACK", "1")
        .env("PYTHONUNBUFFERED", "1")
        .env("PYTHONNOUSERSITE", "1")
        .env("TMPDIR", rt.join("tmp"))
        .env("MACOSX_DEPLOYMENT_TARGET", "15.0")
        .current_dir(&rt)
        .stdin(Stdio::null())
        .kill_on_drop(true);
}
async fn bootstrap(home: &Home) -> anyhow::Result<PathBuf> {
    let _guard = lock(&home.0.join("locks/tools.lock"), false)?;
    let dir = home.0.join(format!("tools/uv-{UV_VERSION}"));
    let binary = dir.join("uv-aarch64-apple-darwin/uv");
    if binary.is_file() {
        return Ok(binary);
    }
    eprintln!("installing private uv {UV_VERSION}");
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(180))
        .build()?;
    let mut response=client.get(format!("https://github.com/astral-sh/uv/releases/download/{UV_VERSION}/uv-aarch64-apple-darwin.tar.gz")).send().await?.error_for_status()?;
    let mut bytes = Vec::new();
    while let Some(chunk) = response.chunk().await? {
        ensure!(
            bytes.len() + chunk.len() <= 64 * 1024 * 1024,
            "bootstrap download too large"
        );
        bytes.extend_from_slice(&chunk);
    }
    ensure!(
        format!("{:x}", Sha256::digest(&bytes)) == UV_SHA,
        "uv checksum mismatch"
    );
    let staging = dir.with_extension("staging");
    if staging.exists() {
        fs::remove_dir_all(&staging)?;
    }
    fs::create_dir_all(&staging)?;
    tar::Archive::new(flate2::read::GzDecoder::new(&bytes[..])).unpack(&staging)?;
    fs::rename(staging, dir)?;
    Ok(binary)
}
async fn checked(cmd: &mut Command, label: &str) -> anyhow::Result<()> {
    ensure!(
        cmd.status()
            .await
            .with_context(|| format!("start {label}"))?
            .success(),
        "{label} failed"
    );
    Ok(())
}
pub async fn install(home: &Home, p: Profile) -> anyhow::Result<Installed> {
    ensure!(
        cfg!(all(target_os = "macos", target_arch = "aarch64")),
        "Apple Silicon macOS is required"
    );
    let rt = home.runtime(p);
    fs::create_dir_all(rt.join("tmp"))?;
    for (name, bytes) in [
        (
            "embedding_server.py",
            &include_bytes!("../../python/embedding_server.py")[..],
        ),
        (
            "stt_server.py",
            &include_bytes!("../../python/stt_server.py")[..],
        ),
        (
            "resources.py",
            &include_bytes!("../../python/resources.py")[..],
        ),
        (
            "http_limits.py",
            &include_bytes!("../../python/http_limits.py")[..],
        ),
    ] {
        atomic_write(&rt.join(name), bytes)?;
    }
    let fingerprint = format!(
        "{:x}",
        Sha256::digest(format!("{PYTHON}:{}:{}", p.revision(), p.lock()))
    );
    let receipt = rt.join("installed.json");
    if let Ok(data) = fs::read(&receipt) {
        let installed: Installed = serde_json::from_slice(&data)?;
        if installed.fingerprint == fingerprint
            && installed.python.is_file()
            && installed.model.is_dir()
        {
            return Ok(installed);
        }
    }
    let uv = bootstrap(home).await?;
    eprintln!("{}: installing isolated Python {PYTHON}", p.id());
    let mut cmd = Command::new(&uv);
    environment(&mut cmd, home, p);
    checked(
        cmd.args(["python", "install", PYTHON, "--no-bin"]),
        "Python install",
    )
    .await?;
    let mut cmd = Command::new(&uv);
    environment(&mut cmd, home, p);
    checked(
        cmd.args([
            "venv",
            "--no-project",
            "--managed-python",
            "--python",
            PYTHON,
        ])
        .arg(rt.join("venv")),
        "venv install",
    )
    .await?;
    let python = rt.join("venv/bin/python");
    atomic_write(&rt.join("requirements.lock"), p.lock().as_bytes())?;
    let mut cmd = Command::new(&uv);
    environment(&mut cmd, home, p);
    checked(
        cmd.args(["pip", "sync", "--require-hashes", "--python"])
            .arg(&python)
            .arg(rt.join("requirements.lock")),
        "locked dependency install",
    )
    .await?;
    eprintln!("{}: downloading {} at {}", p.id(), p.model(), p.revision());
    let mut cmd = Command::new(&python);
    environment(&mut cmd, home, p);
    cmd.env("LLMGATE_MODEL_ID", p.model())
        .env("LLMGATE_MODEL_REVISION", p.revision());
    checked(cmd.args(["-c","import os,pathlib; from huggingface_hub import snapshot_download; p=snapshot_download(os.environ['LLMGATE_MODEL_ID'],revision=os.environ['LLMGATE_MODEL_REVISION']); pathlib.Path('model-path.txt').write_text(p)"]),"model download").await?;
    let model = PathBuf::from(fs::read_to_string(rt.join("model-path.txt"))?.trim());
    ensure!(
        model
            .canonicalize()?
            .starts_with(home.0.join("cache/models").canonicalize()?),
        "model escaped managed cache"
    );
    let installed = Installed {
        python,
        model,
        fingerprint,
    };
    atomic_write(&receipt, &serde_json::to_vec_pretty(&installed)?)?;
    Ok(installed)
}
pub fn load(home: &Home, p: Profile) -> anyhow::Result<Installed> {
    Ok(serde_json::from_slice(&fs::read(
        home.runtime(p).join("installed.json"),
    )?)?)
}
