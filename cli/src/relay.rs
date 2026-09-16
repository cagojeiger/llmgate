use crate::lifecycle::State;
use relaygate_sdk::{AccessTokenSource, Config, Destination, Relay};
use std::{net::SocketAddr, sync::Arc, time::Duration};
use tokio::{
    io::copy_bidirectional,
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
) {
    let (config, destination, token) = route;
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
                let mut tick = tokio::time::interval(Duration::from_secs(1));
                loop {
                    tokio::select! {
                        _=stop.cancelled()=>break,
                        _=tick.tick()=>state.lock().await.publish=format!("{:?}",listener.status()).to_lowercase(),
                        _=tasks.join_next(),if !tasks.is_empty()=>{},
                        result=listener.accept(),if tasks.len()<max=>{
                            match result {
                                Ok(mut pipe)=>{tasks.spawn(async move{let _=timeout(budget,async{let mut upstream=TcpStream::connect(address).await?;copy_bidirectional(&mut pipe,&mut upstream).await}).await;});},
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
        tokio::select! {_=stop.cancelled()=>break,_=sleep(Duration::from_millis(delay*1000+crate::broker::now()%500))=>{}}
        delay = (delay * 2).min(30);
    }
    state.lock().await.publish = "inactive".into();
}
