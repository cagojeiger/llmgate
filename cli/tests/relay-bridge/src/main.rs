mod bridge;
mod config;

use anyhow::ensure;
use relaygate_sdk::Relay;
use tokio_util::sync::CancellationToken;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let mode = std::env::args().nth(1).unwrap_or_default();
    ensure!(
        mode == "connect" && std::env::args().len() == 2,
        "usage: llmgate-relay-probe connect"
    );
    let settings = config::Settings::from_env()?;
    let relay = Relay::connect(settings.sdk.clone()).await?;
    let shutdown = CancellationToken::new();
    let stop = shutdown.clone();
    let signal = tokio::spawn(async move {
        if tokio::signal::ctrl_c().await.is_ok() {
            stop.cancel();
        }
    });
    let result = bridge::connect(settings, relay.clone(), shutdown).await;
    relay.close();
    signal.abort();
    result
}
