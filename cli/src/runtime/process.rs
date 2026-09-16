use super::install::{self, Installed};
use crate::{logs::Logger, profile::Profile, storage::Home};
use anyhow::Context;
use nix::{
    sys::signal::{Signal, killpg},
    unistd::Pid,
};
use std::{
    fs::File,
    net::{SocketAddr, TcpListener},
    os::fd::AsRawFd,
    process::Stdio,
    time::Duration,
};
use tokio::process::{Child, ChildStdin, Command};

pub struct ModelProcess {
    pub child: Child,
    pid: Option<u32>,
    leases: Option<[File; 2]>,
    supervisor_pipe: Option<ChildStdin>,
}
impl ModelProcess {
    pub fn launch(
        home: &Home,
        p: Profile,
        installed: &Installed,
        port: u16,
        log: &Logger,
    ) -> anyhow::Result<Self> {
        let leases = [home.engine_lock(p)?, home.cache_lock(false)?];
        let mut cmd = Command::new(&installed.python);
        cmd.arg(home.runtime(p).join("runtime_guard.py"));
        cmd.arg(home.runtime(p).join(match p {
            Profile::Embedding => "embedding_server.py",
            Profile::Stt => "stt_server.py",
        }));
        install::environment(&mut cmd, home, p);
        cmd.env("LLMGATE_MODEL_PATH", &installed.model)
            .env("LLMGATE_SERVED_MODEL", p.served_name())
            .env("LLMGATE_MODEL_PORT", port.to_string());
        cmd.env("LLMGATE_OWNER_FD", leases[0].as_raw_fd().to_string())
            .env("LLMGATE_CACHE_FD", leases[1].as_raw_fd().to_string());
        cmd.stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .process_group(0);
        use nix::fcntl::{FcntlArg, FdFlag, fcntl};
        // Clear CLOEXEC only for the model spawn, then restore it for other helpers.
        for lease in &leases {
            fcntl(lease, FcntlArg::F_SETFD(FdFlag::empty()))?;
        }
        let spawned = cmd.spawn();
        for lease in &leases {
            fcntl(lease, FcntlArg::F_SETFD(FdFlag::FD_CLOEXEC))?;
        }
        let mut child = spawned.context("start MLX model")?;
        let pid = child.id().context("missing child PID")?;
        // Child::wait closes child.stdin; retain the liveness pipe outside Child.
        let supervisor_pipe = child.stdin.take();
        if let Some(out) = child.stdout.take() {
            log.drain(out)
        }
        if let Some(err) = child.stderr.take() {
            log.drain(err)
        }
        Ok(Self {
            child,
            pid: Some(pid),
            leases: Some(leases),
            supervisor_pipe,
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
        self.supervisor_pipe = None;
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
        self.leases = None;
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
pub async fn ready(client: &reqwest::Client, _p: Profile, port: u16) -> bool {
    let Ok(response) = client
        .get(format!("http://127.0.0.1:{port}/health"))
        .send()
        .await
    else {
        return false;
    };
    response.status().is_success()
        && response
            .json::<serde_json::Value>()
            .await
            .is_ok_and(|v| v.get("ready").and_then(|v| v.as_bool()) == Some(true))
}
pub async fn port_closed(port: u16) -> bool {
    tokio::net::TcpStream::connect(SocketAddr::from(([127, 0, 0, 1], port)))
        .await
        .is_err()
}
