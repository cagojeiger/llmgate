use crate::{lifecycle::State, profile::Profile, storage::Home};
use anyhow::{Context, ensure};
use std::{fs::File, io::ErrorKind};

// Call while holding the profile lock. A child inherits this lease across a
// supervisor crash, so a free lease proves no protected engine is still alive.
pub async fn idle(home: &Home, p: Profile) -> anyhow::Result<File> {
    let lease = home
        .engine_lock(p)
        .context("model process still running; wait for shutdown before retrying")?;
    let path = home.0.join(format!("control/{}.json", p.id()));
    match std::fs::read(path) {
        Ok(bytes) => {
            let state: State = serde_json::from_slice(&bytes)?;
            ensure!(
                state.lease_protected || state.runtime == "stopped",
                "legacy unclean exit; inspect owned process before removing runtime"
            );
            ensure!(
                super::process::port_closed(state.port).await,
                "previous model port remains open; refusing to replace runtime"
            );
        }
        Err(e) if e.kind() == ErrorKind::NotFound => {}
        Err(e) => return Err(e.into()),
    }
    Ok(lease)
}
