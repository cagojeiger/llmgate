mod bridge;
mod config;
mod token;

use anyhow::{Context, ensure};
use relaygate_sdk::{Config, Relay};
use std::{path::PathBuf, sync::Arc, time::Duration};
use tokio::{
    net::TcpListener,
    sync::{Semaphore, watch},
    task::JoinSet,
};
use tokio_util::sync::CancellationToken;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let args: Vec<_> = std::env::args().skip(1).collect();
    if args == ["--version"] {
        println!("llmgate-caller {}", env!("CARGO_PKG_VERSION"));
        return Ok(());
    }
    ensure!(args.is_empty(), "configure with LLMGATE_CALLER_CONFIG");
    let path = PathBuf::from(
        std::env::var_os("LLMGATE_CALLER_CONFIG").context("LLMGATE_CALLER_CONFIG is required")?,
    );
    let config = config::Settings::load(&path)?;
    let mut listeners = Vec::new();
    for (address, destination) in &config.routes {
        listeners.push((TcpListener::bind(address).await?, destination.clone()));
        eprintln!("caller listening on {address}");
    }
    let stop = CancellationToken::new();
    let signal_stop = stop.clone();
    let signal = tokio::spawn(async move {
        let mut term = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .expect("SIGTERM handler");
        tokio::select! { _=tokio::signal::ctrl_c()=>{}, _=term.recv()=>{} }
        signal_stop.cancel();
    });
    let (sender, receiver) = watch::channel(None);
    let manager = tokio::spawn(connect(config.sdk, sender, stop.clone()));
    let token = token::source(
        config.issuer,
        config.routes.iter().map(|(_, d)| d.clone()).collect(),
        config.ttl,
    );
    let slots = Arc::new(Semaphore::new(config.max));
    let mut servers = JoinSet::new();
    for (listener, destination) in listeners {
        servers.spawn(bridge::serve(
            listener,
            destination,
            token.clone(),
            receiver.clone(),
            slots.clone(),
            config.timeout,
            stop.clone(),
        ));
    }
    let mut result = Ok(());
    while let Some(done) = servers.join_next().await {
        match done {
            Ok(Ok(())) => {}
            Ok(Err(e)) => {
                result = Err(e);
                stop.cancel();
            }
            Err(e) => {
                result = Err(e.into());
                stop.cancel();
            }
        }
    }
    stop.cancel();
    manager.await?;
    signal.abort();
    result
}

async fn connect(config: Config, sender: watch::Sender<Option<Relay>>, stop: CancellationToken) {
    loop {
        let result =
            tokio::select! { _=stop.cancelled()=>return, r=Relay::connect(config.clone())=>r };
        if let Ok(relay) = result {
            sender.send_replace(Some(relay.clone()));
            // The SDK owns reconnect after first connection, including Gateway restarts.
            stop.cancelled().await;
            relay.close();
            sender.send_replace(None);
            return;
        }
        eprintln!("Gateway not ready; retrying initial connection");
        tokio::select! { _=stop.cancelled()=>return, _=tokio::time::sleep(Duration::from_secs(2))=>{} }
    }
}
