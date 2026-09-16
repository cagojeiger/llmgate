use crate::{
    broker::{self, Registration},
    logs::Logger,
    profile::Profile,
    runtime::{
        install, ownership,
        process::{self, ModelProcess},
    },
    storage::Home,
};
use anyhow::{Context, ensure};
use serde::{Deserialize, Serialize};
use std::{
    fs,
    os::unix::fs::PermissionsExt,
    sync::{Arc, atomic::Ordering},
    time::Duration,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{UnixListener, UnixStream},
    sync::Mutex,
    time::{sleep, timeout},
};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct State {
    pub profile: String,
    pub runtime: String,
    pub publish: String,
    pub port: u16,
    pub model: String,
    pub revision: String,
    pub restarts: u32,
    pub dropped_log_chunks: u64,
    pub error: Option<String>,
    #[serde(default)]
    pub lease_protected: bool,
}
#[derive(Clone, clap::Args)]
pub struct StartOptions {
    #[arg(value_enum)]
    pub profile: Option<Profile>,
    /// Start registered profiles (both profiles with --local-only).
    #[arg(long)]
    pub all: bool,
    #[arg(long)]
    pub foreground: bool,
    #[arg(long)]
    pub local_only: bool,
    #[arg(long,value_parser=clap::value_parser!(u16).range(1..))]
    pub port: Option<u16>,
    #[arg(long,default_value_t=8,value_parser=clap::value_parser!(u16).range(1..=64))]
    pub max_connections: u16,
    #[arg(long,default_value_t=3600,value_parser=clap::value_parser!(u16).range(1..=3600))]
    pub connection_timeout: u16,
    /// Seconds to wait for model readiness and Relay publication, after installation.
    #[arg(long, default_value_t=660, value_parser=clap::value_parser!(u16).range(1..=3600))]
    pub wait_timeout: u16,
}
pub async fn control(home: &Home, p: Profile, command: &str) -> anyhow::Result<String> {
    timeout(Duration::from_secs(3), async {
        let mut socket = UnixStream::connect(home.socket(p)).await?;
        socket.write_all(command.as_bytes()).await?;
        socket.shutdown().await?;
        let mut out = String::new();
        socket.take(16 * 1024).read_to_string(&mut out).await?;
        Ok::<_, anyhow::Error>(out)
    })
    .await?
}
pub async fn stop(home: &Home, p: Profile) -> anyhow::Result<()> {
    if control(home, p, "stop").await.is_ok() {
        for _ in 0..150 {
            if home.profile_lock(p).is_ok() {
                break;
            }
            sleep(Duration::from_millis(200)).await;
        }
    }
    let _lock = home
        .profile_lock(p)
        .context("profile still running; cleanup not complete")?;
    let _engine = ownership::idle(home, p).await?;
    let rt = home.runtime(p);
    if rt.exists() {
        fs::remove_dir_all(rt)?;
    }
    let _ = fs::remove_file(home.socket(p));
    Ok(())
}

async fn run_inner(
    home: Home,
    p: Profile,
    opts: StartOptions,
    stop: CancellationToken,
) -> anyhow::Result<()> {
    let _profile_lock = home.profile_lock(p)?;
    let _cache_lock = home.cache_lock(false)?;
    drop(ownership::idle(&home, p).await?);
    let installed = install::load(&home, p)?;
    let mut grant_config = None;
    if !opts.local_only {
        crate::logs::supervisor_event(
            home.0.join(format!("logs/{}-supervisor.log", p.id())),
            "worker_auth_start",
        )?;
        let registration: Registration = home.read("registration.json")?;
        ensure!(registration.profiles.contains(&p), "profile not registered");
        let client = broker::client(&registration)?;
        let key = broker::load_key(&home)?;
        let grant = broker::grant(&client, &registration, &key, p).await?;
        let mut sdk = relaygate_sdk::Config::new(&grant.gateway_endpoint)?;
        if let Some(ca) = &registration.ca_file {
            sdk = sdk.with_ca_certificate(&fs::read(ca)?)?;
        }
        let destination = grant.destination.parse()?;
        let token = broker::source(client, registration, key, p, grant);
        grant_config = Some((sdk, destination, token));
    }
    let state = Arc::new(Mutex::new(State {
        profile: p.id().into(),
        runtime: "starting".into(),
        publish: if opts.local_only {
            "disabled"
        } else {
            "inactive"
        }
        .into(),
        port: 0,
        model: p.model().into(),
        revision: p.revision().into(),
        restarts: 0,
        dropped_log_chunks: 0,
        error: None,
        lease_protected: true,
    }));
    let ctl_stop = stop.clone();
    let ctl_state = state.clone();
    let _ = fs::remove_file(home.socket(p));
    let listener = UnixListener::bind(home.socket(p))?;
    fs::set_permissions(home.socket(p), fs::Permissions::from_mode(0o600))?;
    let control_task = tokio::spawn(async move {
        loop {
            tokio::select! {
                _=ctl_stop.cancelled()=>break,
                incoming=listener.accept()=>if let Ok((mut socket,_))=incoming {
                    let state=ctl_state.clone();let stop=ctl_stop.clone();
                    tokio::spawn(async move{
                        let mut request=String::new();
                        if timeout(Duration::from_secs(2),(&mut socket).take(64).read_to_string(&mut request)).await.is_ok(){
                            if request=="stop"{stop.cancel();}
                            if let Ok(bytes)=serde_json::to_vec(&*state.lock().await){let _=socket.write_all(&bytes).await;}
                        }
                    });
                }
            }
        }
    });
    let signal_stop = stop.clone();
    let signal_task = tokio::spawn(async move {
        let mut term =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()).ok();
        tokio::select! {_=tokio::signal::ctrl_c()=>{},_=async{if let Some(t)=term.as_mut(){t.recv().await;}else{std::future::pending::<()>().await}}=>{}}
        signal_stop.cancel();
    });
    let log = Logger::start(home.0.join(format!("logs/{}.log", p.id())));
    let client = reqwest::Client::builder()
        // Engine health is always local, regardless of operator proxy settings.
        .no_proxy()
        .timeout(Duration::from_secs(3))
        .build()?;
    let result = supervise(
        &home,
        p,
        &opts,
        &installed,
        &state,
        &stop,
        &log,
        &client,
        grant_config,
    )
    .await;
    stop.cancel();
    control_task.abort();
    signal_task.abort();
    {
        let mut s = state.lock().await;
        s.runtime = if result.is_ok() {
            "stopped"
        } else {
            "cleanup_failed"
        }
        .into();
        s.publish = "inactive".into();
        s.error = result.as_ref().err().map(|e| e.to_string());
        home.write(&format!("control/{}.json", p.id()), &*s)?;
    }
    let _ = fs::remove_file(home.socket(p));
    if result.is_ok() {
        fs::remove_dir_all(home.runtime(p))?;
    }
    result
}

type RelayConfig = (
    relaygate_sdk::Config,
    relaygate_sdk::Destination,
    relaygate_sdk::AccessTokenSource,
);
#[allow(clippy::too_many_arguments)]
async fn supervise(
    home: &Home,
    p: Profile,
    opts: &StartOptions,
    installed: &install::Installed,
    state: &Arc<Mutex<State>>,
    stop: &CancellationToken,
    log: &Logger,
    client: &reqwest::Client,
    grant: Option<RelayConfig>,
) -> anyhow::Result<()> {
    for attempt in 0..4 {
        if stop.is_cancelled() {
            return Ok(());
        }
        let port = process::available_port(opts.port)?;
        {
            let mut s = state.lock().await;
            s.port = port;
            s.runtime = "starting".into();
            s.restarts = attempt;
            home.write(&format!("control/{}.json", p.id()), &*s)?;
        }
        crate::logs::supervisor_event(
            home.0.join(format!("logs/{}-supervisor.log", p.id())),
            "model_start",
        )?;
        let mut model = ModelProcess::launch(home, p, installed, port, log)?;
        let began = tokio::time::Instant::now();
        let mut ready = false;
        while began.elapsed() < Duration::from_secs(600) && !stop.is_cancelled() {
            if model.child.try_wait()?.is_some() {
                break;
            }
            if process::ready(client, p, port).await && model.owns_port(port).await {
                // Python only serves health after loading and warming the model.
                ready = true;
                break;
            }
            tokio::select! {_=stop.cancelled()=>break,_=sleep(Duration::from_millis(500))=>{}}
        }
        let published_stop = CancellationToken::new();
        let _withdraw_on_error = published_stop.clone().drop_guard();
        let mut publishing = None;
        if ready && !stop.is_cancelled() {
            state.lock().await.runtime = "ready".into();
            crate::logs::supervisor_event(
                home.0.join(format!("logs/{}-supervisor.log", p.id())),
                "model_ready",
            )?;
            if let Some((sdk, dest, token)) = &grant {
                publishing = Some(tokio::spawn(crate::relay::publish(
                    (sdk.clone(), dest.clone(), token.clone()),
                    ([127, 0, 0, 1], port).into(),
                    opts.max_connections as usize,
                    Duration::from_secs(opts.connection_timeout as u64),
                    state.clone(),
                    published_stop.clone(),
                    log.clone(),
                )));
            }
            let mut health = tokio::time::interval(Duration::from_secs(10));
            loop {
                tokio::select! {
                    _=stop.cancelled()=>break,
                    _=model.child.wait()=>break,
                    _=health.tick()=>{
                        if !process::ready(client,p,port).await{
                            let _ = crate::logs::supervisor_event(home.0.join(format!("logs/{}-supervisor.log",p.id())), "model_health_failed");
                            break
                        }
                        let mut s=state.lock().await;s.dropped_log_chunks=log.dropped.load(Ordering::Relaxed);home.write(&format!("control/{}.json",p.id()),&*s)?;
                    }
                }
            }
        }
        state.lock().await.runtime = "stopping".into();
        published_stop.cancel();
        if let Some(mut task) = publishing
            && timeout(Duration::from_secs(12), &mut task).await.is_err()
        {
            task.abort();
            let _ = task.await;
        }
        model.stop().await?;
        ensure!(
            process::port_closed(port).await,
            "model port still open after shutdown"
        );
        if stop.is_cancelled() {
            return Ok(());
        }
        state.lock().await.runtime = "unhealthy".into();
        let _ = crate::logs::supervisor_event(
            home.0.join(format!("logs/{}-supervisor.log", p.id())),
            "model_restart",
        );
        if opts.port.is_some() && !ready {
            anyhow::bail!("model startup failed; inspect profile log")
        }
        tokio::select! {_=stop.cancelled()=>return Ok(()),_=sleep(Duration::from_secs(1<<attempt))=>{}}
    }
    anyhow::bail!("model restart budget exhausted; inspect profile log")
}

// Keep lifecycle diagnostics bounded and free of raw token/provider errors.
pub async fn run(
    home: Home,
    p: Profile,
    opts: StartOptions,
    stop: CancellationToken,
) -> anyhow::Result<()> {
    let path = home.0.join(format!("logs/{}-supervisor.log", p.id()));
    crate::logs::supervisor_event(path.clone(), "supervisor_start")?;
    let result = run_inner(home, p, opts, stop).await;
    if let Err(error) = &result
        && let Some(rejected) = error.downcast_ref::<crate::broker::GrantRejected>()
    {
        let _ = crate::logs::supervisor_event(
            path.clone(),
            &format!(
                "worker_auth_rejected http_status={} action=check_key_and_worker_permissions",
                rejected.status
            ),
        );
    }
    let event = if result.is_ok() {
        "supervisor_stopped"
    } else {
        "supervisor_failed"
    };
    if crate::logs::supervisor_event(path, event).is_err() {
        eprintln!("supervisor_log_write_failed");
    }
    result
}
