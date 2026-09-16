use super::install::{self, Installed};
use crate::{logs::Logger, profile::Profile, storage::Home};
use anyhow::{Context, ensure};
use nix::{
    sys::signal::{Signal, killpg},
    unistd::Pid,
};
use std::{
    net::{SocketAddr, TcpListener},
    process::Stdio,
    time::Duration,
};
use tokio::process::{Child, Command};

pub struct ModelProcess {
    pub child: Child,
    pid: Option<u32>,
}
impl ModelProcess {
    pub fn launch(
        home: &Home,
        p: Profile,
        installed: &Installed,
        port: u16,
        log: &Logger,
    ) -> anyhow::Result<Self> {
        let mut cmd = Command::new(&installed.python);
        cmd.arg(home.runtime(p).join(match p {
            Profile::Embedding => "embedding_server.py",
            Profile::Stt => "stt_server.py",
        }));
        install::environment(&mut cmd, home, p);
        cmd.env("LLMGATE_MODEL_PATH", &installed.model)
            .env("LLMGATE_SERVED_MODEL", p.served_name())
            .env("LLMGATE_MODEL_PORT", port.to_string());
        cmd.stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .process_group(0);
        let mut child = cmd.spawn().context("start MLX model")?;
        let pid = child.id().context("missing child PID")?;
        if let Some(out) = child.stdout.take() {
            log.drain(out)
        }
        if let Some(err) = child.stderr.take() {
            log.drain(err)
        }
        Ok(Self {
            child,
            pid: Some(pid),
        })
    }
    pub async fn owns_port(&self, port: u16) -> bool {
        let Some(pid) = self.pid else { return false };
        let Ok(listeners) = Command::new("/usr/sbin/lsof")
            .args(["-nP", "-t", &format!("-iTCP:{port}"), "-sTCP:LISTEN"])
            .output()
            .await
        else {
            return false;
        };
        for owner in String::from_utf8_lossy(&listeners.stdout).split_whitespace() {
            if let Ok(result) = Command::new("/bin/ps")
                .args(["-o", "pgid=", "-p", owner])
                .output()
                .await
                && String::from_utf8_lossy(&result.stdout).trim() == pid.to_string()
            {
                return true;
            }
        }
        false
    }
    pub async fn stop(&mut self) -> anyhow::Result<()> {
        let Some(pid) = self.pid else { return Ok(()) };
        let _ = killpg(Pid::from_raw(pid as i32), Signal::SIGTERM);
        if tokio::time::timeout(Duration::from_secs(10), self.child.wait())
            .await
            .is_err()
        {
            let _ = killpg(Pid::from_raw(pid as i32), Signal::SIGKILL);
            self.child.wait().await?;
        }
        // Close owned descendants too; the supervisor never signals an arbitrary stored PID.
        let _ = killpg(Pid::from_raw(pid as i32), Signal::SIGKILL);
        self.pid = None;
        Ok(())
    }
}
impl Drop for ModelProcess {
    fn drop(&mut self) {
        if let Some(pid) = self.pid {
            let _ = killpg(Pid::from_raw(pid as i32), Signal::SIGKILL);
        }
    }
}
pub fn available_port(requested: Option<u16>) -> anyhow::Result<u16> {
    let listener = TcpListener::bind(("127.0.0.1", requested.unwrap_or(0)))
        .context("model port already in use")?;
    Ok(listener.local_addr()?.port())
}
pub async fn ready(client: &reqwest::Client, p: Profile, port: u16) -> bool {
    client
        .get(format!(
            "http://127.0.0.1:{port}/{}",
            if p == Profile::Stt {
                "health"
            } else {
                "v1/models"
            }
        ))
        .send()
        .await
        .is_ok_and(|r| r.status().is_success())
}
pub async fn warmup(client: &reqwest::Client, p: Profile, port: u16) -> anyhow::Result<()> {
    if p == Profile::Stt {
        return Ok(());
    }
    let response = client
        .post(format!("http://127.0.0.1:{port}/v1/embeddings"))
        .json(&serde_json::json!({"model":p.served_name(),"input":["ready"]}))
        .timeout(Duration::from_secs(180))
        .send()
        .await?;
    ensure!(
        response.status().is_success(),
        "model warmup failed (HTTP {})",
        response.status()
    );
    let _: serde_json::Value = response.json().await?;
    Ok(())
}
pub async fn port_closed(port: u16) -> bool {
    tokio::net::TcpStream::connect(SocketAddr::from(([127, 0, 0, 1], port)))
        .await
        .is_err()
}
