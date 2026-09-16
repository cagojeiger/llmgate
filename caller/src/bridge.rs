use relaygate_sdk::{AccessTokenSource, Destination, Relay};
use std::{sync::Arc, time::Duration};
use tokio::{
    io::{AsyncWriteExt, copy_bidirectional_with_sizes},
    net::{TcpListener, TcpStream},
    sync::{Semaphore, watch},
    task::JoinSet,
    time::timeout,
};
use tokio_util::sync::CancellationToken;

pub async fn serve(
    listener: TcpListener,
    destination: Destination,
    token: AccessTokenSource,
    relay: watch::Receiver<Option<Relay>>,
    slots: Arc<Semaphore>,
    budget: Duration,
    stop: CancellationToken,
) -> anyhow::Result<()> {
    let mut tasks = JoinSet::new();
    // Match SDK 64 KiB DATA chunks; 8 KiB copies amplify large audio into queue bursts.
    loop {
        tokio::select! {
            _ = stop.cancelled() => break,
            _ = tasks.join_next(), if !tasks.is_empty() => {},
            accepted = listener.accept() => {
                let (mut socket, _) = accepted?;
                let Ok(permit) = slots.clone().try_acquire_owned() else {
                    unavailable(&mut socket).await;
                    continue;
                };
                let current = relay.borrow().clone();
                let destination = destination.clone();
                let token = token.clone();
                tasks.spawn(async move {
                    let _permit = permit;
                    let _ = timeout(budget, async {
                        let Some(relay) = current else { unavailable(&mut socket).await; return; };
                        match relay.dial(destination, token).await {
                            Ok(mut pipe) => { let _ = copy_bidirectional_with_sizes(&mut socket, &mut pipe, 64 * 1024, 64 * 1024).await; }
                            Err(_) => unavailable(&mut socket).await,
                        }
                    }).await;
                });
            }
        }
    }
    if timeout(Duration::from_secs(5), async {
        while tasks.join_next().await.is_some() {}
    })
    .await
    .is_err()
    {
        tasks.shutdown().await;
    }
    Ok(())
}

async fn unavailable(socket: &mut TcpStream) {
    // Only before any Pipe payload is forwarded. Never replay a failed request.
    let body = r#"{"error":{"message":"local worker unavailable","type":"upstream_error"}}"#;
    let response = format!(
        "HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\nRetry-After: 1\r\n\r\n{}",
        body.len(),
        body
    );
    let _ = timeout(Duration::from_secs(1), async {
        socket.write_all(response.as_bytes()).await?;
        socket.shutdown().await
    })
    .await;
}
