use relaygate_sdk::{AccessToken, AccessTokenSource, Config, Destination, ErrorCode, Relay};
use std::{sync::Arc, time::Duration};
use tokio::{
    net::TcpListener,
    sync::Mutex,
    time::{sleep, timeout},
};
use tokio_util::sync::CancellationToken;

// Compiles the production worker bridge, not a parallel implementation.
#[path = "../../../src/relay.rs"]
mod worker;

pub async fn verify() -> anyhow::Result<()> {
    let config = Config::new(std::env::var("RELAYGATE_ADDR")?)?
        .with_ca_certificate(&std::fs::read(std::env::var("RELAYGATE_CA_FILE")?)?)?;
    let destination: Destination = "llmgate/capacity-v1".parse()?;
    let engine = TcpListener::bind("127.0.0.1:0").await?;
    let state = Arc::new(Mutex::new(crate::lifecycle::State {
        publish: String::new(),
    }));
    let stop = CancellationToken::new();
    let task = tokio::spawn(worker::publish(
        (
            config.clone(),
            destination.clone(),
            AccessTokenSource::static_token(AccessToken::new(std::env::var("PUBLISH_TOKEN")?)?),
        ),
        engine.local_addr()?,
        1,
        Duration::from_secs(30),
        state.clone(),
        stop.clone(),
    ));
    let outcome = async {
        timeout(Duration::from_secs(10), async {
            while state.lock().await.publish != "active" {
                sleep(Duration::from_millis(20)).await;
            }
        })
        .await?;
        let caller = Relay::connect(config).await?;
        let token =
            AccessTokenSource::static_token(AccessToken::new(std::env::var("DIAL_TOKEN")?)?);
        let first = caller.dial(destination.clone(), token.clone()).await?;
        let (upstream, _) = timeout(Duration::from_secs(3), engine.accept()).await??;
        let second = caller.dial(destination, token).await;
        anyhow::ensure!(
            second
                .as_ref()
                .is_err_and(|e| e.code() == ErrorCode::ResourceExhausted),
            "second Pipe must be rejected while max_connections=1"
        );
        drop(second);
        drop(first);
        drop(upstream);
        caller.close();
        Ok::<(), anyhow::Error>(())
    }
    .await;
    stop.cancel();
    task.await?;
    outcome?;
    println!("worker max_connections=1: second Pipe rejected with RESOURCE_EXHAUSTED");
    Ok(())
}
