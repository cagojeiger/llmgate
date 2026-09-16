use crate::lifecycle::State;
use relaygate_sdk::{AccessTokenSource, Config, Destination, Relay, ResourceLimits};
use std::{net::SocketAddr, sync::Arc, time::Duration};
use tokio::{
    io::copy_bidirectional_with_sizes,
    net::TcpStream,
    sync::Mutex,
    task::JoinSet,
    time::{sleep, timeout},
};
use tokio_util::sync::CancellationToken;

pub async fn publish(
    route: (Config, Destination, AccessTokenSource),
    address: SocketAddr,
    max: usize,
    budget: Duration,
    state: Arc<Mutex<State>>,
    stop: CancellationToken,
    log: crate::logs::Logger,
) {
    let (config, destination, token) = route;
    let config = config.with_resource_limits(
        ResourceLimits::default()
            .with_max_pending_pipes_per_listener(max)
            .with_max_live_pipes_per_listener(max)
            .with_max_live_pipes_per_relay(max),
    );
    let mut delay = 1u64;
    loop {
        if stop.is_cancelled() {
            break;
        }
        state.lock().await.publish = "connecting".into();
        let connected =
            tokio::select! {_ = stop.cancelled()=>break,r=Relay::connect(config.clone())=>r};
        if let Ok(relay) = connected {
            let result = tokio::select! {_ = stop.cancelled()=>{relay.close();break},r=relay.listen(destination.clone(),token.clone())=>r};
            if let Ok(listener) = result {
                let mut tasks = JoinSet::new();
                // Match SDK 64 KiB DATA chunks; 8 KiB copies amplify large audio into queue bursts.
                let mut tick = tokio::time::interval(Duration::from_secs(1));
                loop {
                    tokio::select! {
                        _=stop.cancelled()=>break,
                        _=tick.tick()=>state.lock().await.publish=format!("{:?}",listener.status()).to_lowercase(),
                        _=tasks.join_next(),if !tasks.is_empty()=>{},
                        result=listener.accept(),if tasks.len()<max=>{
                            match result {
                                Ok(mut pipe)=>{let log=log.clone();tasks.spawn(async move{
                                    match timeout(budget,async{let mut upstream=TcpStream::connect(address).await?;copy_bidirectional_with_sizes(&mut pipe,&mut upstream,64*1024,64*1024).await}).await {
                                        Err(_)=>log.event("pipe_timeout"),
                                        Ok(Err(_))=>log.event("pipe_io_failed"),
                                        Ok(Ok(_))=>{},
                                    }
                                });},
                                Err(_)=>break,
                            }
                        }
                    }
                }
                let _ = timeout(Duration::from_secs(5), listener.close()).await;
                let drained = timeout(Duration::from_secs(5), async {
                    while tasks.join_next().await.is_some() {}
                })
                .await;
                if drained.is_err() {
                    tasks.shutdown().await;
                }
            }
            relay.close();
        }
        if stop.is_cancelled() {
            break;
        }
        state.lock().await.publish = "retrying".into();
        log.event("relay_retry");
        tokio::select! {_=stop.cancelled()=>break,_=sleep(Duration::from_millis(delay*1000+crate::broker::now()%500))=>{}}
        delay = (delay * 2).min(30);
    }
    state.lock().await.publish = "inactive".into();
}
