use std::{net::SocketAddr, time::Duration};

use anyhow::{Context, ensure};
use relaygate_sdk::{AccessToken, AccessTokenSource, Config, Destination, ResourceLimits};

pub(crate) struct Settings {
    pub(crate) sdk: Config,
    pub(crate) destination: Destination,
    pub(crate) token: AccessTokenSource,
    pub(crate) address: SocketAddr,
    pub(crate) max_connections: usize,
    pub(crate) connection_timeout: Duration,
}

impl Settings {
    pub(crate) fn from_env() -> anyhow::Result<Self> {
        let mut sdk = Config::new(required("RELAYGATE_ADDR")?)?;
        if let Ok(path) = std::env::var("RELAYGATE_CA_FILE") {
            sdk = sdk.with_ca_certificate(&std::fs::read(path)?)?;
        }
        let max_connections: usize = std::env::var("BRIDGE_MAX_CONNECTIONS")
            .unwrap_or_else(|_| "8".into())
            .parse()?;
        ensure!(
            (1..=1024).contains(&max_connections),
            "BRIDGE_MAX_CONNECTIONS must be 1..1024"
        );
        let seconds: u64 = std::env::var("BRIDGE_CONNECTION_TIMEOUT_SECONDS")
            .unwrap_or_else(|_| "120".into())
            .parse()?;
        ensure!(
            (1..=3600).contains(&seconds),
            "connection timeout must be 1..3600 seconds"
        );
        let variable = "BRIDGE_LISTEN";
        let address: SocketAddr = required(variable)?.parse()?;
        ensure!(
            address.ip().is_loopback(),
            "{variable} must be a numeric loopback address"
        );
        let limits = ResourceLimits::default()
            .with_max_pending_pipes_per_listener(max_connections)
            .with_max_live_pipes_per_listener(max_connections)
            .with_max_live_pipes_per_relay(max_connections);
        Ok(Self {
            sdk: sdk.with_resource_limits(limits),
            destination: required("RELAYGATE_DESTINATION")?.parse()?,
            token: AccessTokenSource::static_token(AccessToken::new(required(
                "RELAYGATE_ACCESS_TOKEN",
            )?)?),
            address,
            max_connections,
            connection_timeout: Duration::from_secs(seconds),
        })
    }
}

fn required(name: &str) -> anyhow::Result<String> {
    std::env::var(name).with_context(|| format!("{name} is required"))
}
