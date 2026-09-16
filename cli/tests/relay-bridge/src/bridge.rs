use relaygate_sdk::Relay;
use tokio::{io::copy_bidirectional, net::TcpListener, task::JoinSet, time::timeout};
use tokio_util::sync::CancellationToken;

use crate::config::Settings;

pub(crate) async fn connect(
    settings: Settings,
    relay: Relay,
    shutdown: CancellationToken,
) -> anyhow::Result<()> {
    let listener = TcpListener::bind(settings.address).await?;
    serve_connected(settings, relay, shutdown, listener).await
}

async fn serve_connected(
    settings: Settings,
    relay: Relay,
    shutdown: CancellationToken,
    listener: TcpListener,
) -> anyhow::Result<()> {
    eprintln!("bridge listening on {}", listener.local_addr()?);
    let mut tasks = JoinSet::new();
    loop {
        tokio::select! {
            _ = shutdown.cancelled() => break,
            result = tasks.join_next(), if !tasks.is_empty() => report(result),
            accepted = listener.accept(), if tasks.len() < settings.max_connections => {
                let (mut socket, _) = accepted?;
                let relay = relay.clone();
                let destination = settings.destination.clone();
                let token = settings.token.clone();
                let budget = settings.connection_timeout;
                tasks.spawn(async move {
                    timeout(budget, async {
                        let mut pipe = relay.dial(destination, token).await?;
                        copy_bidirectional(&mut socket, &mut pipe).await?;
                        Ok::<_, anyhow::Error>(())
                    }).await?
                });
            }
        }
    }
    tasks.shutdown().await;
    Ok(())
}

fn report(result: Option<Result<anyhow::Result<()>, tokio::task::JoinError>>) {
    match result {
        Some(Ok(Err(error))) => eprintln!("bridge connection failed: {error}"),
        Some(Err(error)) => eprintln!("bridge task failed: {error}"),
        _ => {}
    }
}
