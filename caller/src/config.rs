use anyhow::{Context, ensure};
use relaygate_sdk::{Config, Destination, ResourceLimits};
use relaygate_token_issuer::TokenIssuer;
use serde::Deserialize;
use std::{
    collections::BTreeMap,
    fs::File,
    io::Read,
    net::SocketAddr,
    path::{Path, PathBuf},
    time::Duration,
};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Settings {
    pub issuer_config: PathBuf,
    pub ca_file: Option<PathBuf>,
    pub gateway_endpoint: Option<String>,
    #[serde(default)]
    pub routes: BTreeMap<String, SocketAddr>,
    #[serde(default = "default_max")]
    pub max_connections: usize,
    #[serde(default = "default_timeout")]
    pub connection_timeout_seconds: u64,
}
fn default_max() -> usize {
    64
}
fn default_timeout() -> u64 {
    180
}

// Same operator-owned issuer/profile file as Go; no API key or token endpoint
// is added. Only this backend process can read the signing key.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct IssuerConfig {
    issuer: String,
    audience: String,
    key_id: String,
    private_key_file: PathBuf,
    gateway_endpoint: String,
    #[serde(default = "default_ttl")]
    ttl_seconds: u64,
    profiles: BTreeMap<String, Profile>,
}
fn default_ttl() -> u64 {
    300
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Profile {
    caller_address: Option<SocketAddr>,
    destination: String,
    version: String,
}

pub struct Prepared {
    pub sdk: Config,
    pub issuer: TokenIssuer,
    pub ttl: Duration,
    pub routes: Vec<(SocketAddr, Destination)>,
    pub max: usize,
    pub timeout: Duration,
}

pub fn bounded_file(path: &Path) -> anyhow::Result<Vec<u8>> {
    let mut bytes = Vec::new();
    File::open(path)
        .context("open caller configuration")?
        .take(65537)
        .read_to_end(&mut bytes)?;
    ensure!(bytes.len() <= 65536, "caller configuration too large");
    Ok(bytes)
}

impl Settings {
    pub fn load(path: &Path) -> anyhow::Result<Prepared> {
        let mut settings: Self = serde_json::from_slice(&bounded_file(path)?)?;
        ensure!(
            (1..=1024).contains(&settings.max_connections),
            "max_connections must be 1..1024"
        );
        ensure!(
            (1..=3600).contains(&settings.connection_timeout_seconds),
            "connection timeout must be 1..3600 seconds"
        );
        let issuer: IssuerConfig = serde_json::from_slice(&bounded_file(&settings.issuer_config)?)?;
        if settings.routes.is_empty() {
            for (name, profile) in &issuer.profiles {
                let default = match name.as_str() {
                    "embedding" => "127.0.0.1:18081",
                    "stt" => "127.0.0.1:18082",
                    _ => anyhow::bail!("unsupported automatic caller profile"),
                };
                settings.routes.insert(
                    name.clone(),
                    profile.caller_address.unwrap_or(default.parse()?),
                );
            }
        }
        ensure!(
            !settings.routes.is_empty() && settings.routes.len() <= 16,
            "caller needs 1..16 routes"
        );
        ensure!(
            (60..=900).contains(&issuer.ttl_seconds),
            "token TTL must be 60..900 seconds"
        );
        let endpoint = settings
            .gateway_endpoint
            .as_ref()
            .unwrap_or(&issuer.gateway_endpoint);
        ensure!(
            endpoint.starts_with("tls://"),
            "caller requires Gateway TLS"
        );
        let mut sdk = Config::new(endpoint)?;
        if let Some(path) = &settings.ca_file {
            sdk = sdk.with_ca_certificate(&bounded_file(path)?)?;
        }
        let max = settings.max_connections;
        sdk = sdk.with_resource_limits(
            ResourceLimits::default()
                .with_max_pending_pipes_per_listener(max)
                .with_max_live_pipes_per_listener(max)
                .with_max_live_pipes_per_relay(max),
        );
        let mut routes = Vec::new();
        for (name, address) in settings.routes {
            ensure!(
                address.ip().is_loopback() && address.port() != 0,
                "caller binds numeric loopback ports only"
            );
            let profile = issuer
                .profiles
                .get(&name)
                .context("unknown caller profile")?;
            ensure!(profile.version == "1", "unsupported caller profile version");
            let expected = match name.as_str() {
                "embedding" => Some("127.0.0.1:18081".parse::<SocketAddr>()?),
                "stt" => Some("127.0.0.1:18082".parse::<SocketAddr>()?),
                _ => None,
            };
            if let Some(expected) = profile.caller_address.or(expected) {
                ensure!(
                    address == expected,
                    "caller route must match workers.json caller_address"
                );
            }
            ensure!(
                !routes.iter().any(|(a, _)| *a == address),
                "duplicate caller bind address"
            );
            routes.push((address, profile.destination.parse()?));
        }
        Ok(Prepared {
            sdk,
            issuer: TokenIssuer::from_es256_pem(
                issuer.issuer,
                issuer.audience,
                issuer.key_id,
                bounded_file(&issuer.private_key_file)?,
            )?,
            ttl: Duration::from_secs(issuer.ttl_seconds),
            routes,
            max,
            timeout: Duration::from_secs(settings.connection_timeout_seconds),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use p256::{
        SecretKey,
        pkcs8::{EncodePrivateKey, LineEnding},
    };
    use serde_json::json;

    #[test]
    fn config_rejects_exposure_and_unknown_routes() -> anyhow::Result<()> {
        let root = tempfile::tempdir()?;
        let key = SecretKey::from_slice(&[1; 32])?.to_pkcs8_pem(LineEnding::LF)?;
        std::fs::write(root.path().join("key.pem"), key.as_bytes())?;
        let issuer = json!({"issuer":"test","audience":"relaygate","key_id":"test",
            "private_key_file":root.path().join("key.pem"),"gateway_endpoint":"tls://localhost:443",
            "ttl_seconds":60,"profiles":{"embedding":{"destination":"llmgate/embedding-v1","version":"1"}}});
        std::fs::write(
            root.path().join("issuer.json"),
            serde_json::to_vec(&issuer)?,
        )?;
        let path = root.path().join("caller.json");
        let mut config = json!({"issuer_config":root.path().join("issuer.json"),"routes":{"embedding":"127.0.0.1:18081"}});
        std::fs::write(&path, serde_json::to_vec(&config)?)?;
        assert!(Settings::load(&path).is_ok());
        config.as_object_mut().unwrap().remove("routes");
        std::fs::write(&path, serde_json::to_vec(&config)?)?;
        let automatic = Settings::load(&path)?;
        assert_eq!(
            automatic.routes[0].0,
            "127.0.0.1:18081".parse::<SocketAddr>()?
        );
        let mut custom = issuer.clone();
        custom["profiles"]["embedding"]["caller_address"] = json!("127.0.0.1:19081");
        std::fs::write(
            root.path().join("issuer.json"),
            serde_json::to_vec(&custom)?,
        )?;
        assert_eq!(
            Settings::load(&path)?.routes[0].0,
            "127.0.0.1:19081".parse::<SocketAddr>()?
        );
        config["routes"] = json!({"embedding":"127.0.0.1:18081"});
        std::fs::write(&path, serde_json::to_vec(&config)?)?;
        assert!(Settings::load(&path).is_err());
        std::fs::write(
            root.path().join("issuer.json"),
            serde_json::to_vec(&issuer)?,
        )?;

        config["routes"]["embedding"] = json!("0.0.0.0:18081");
        std::fs::write(&path, serde_json::to_vec(&config)?)?;
        assert!(Settings::load(&path).is_err());
        config["routes"] = json!({"unregistered":"127.0.0.1:18081"});
        std::fs::write(&path, serde_json::to_vec(&config)?)?;
        assert!(Settings::load(&path).is_err());
        config["routes"] = json!({"embedding":"127.0.0.1:18081"});
        config["gateway_endpoint"] = json!("tcp://localhost:443");
        std::fs::write(&path, serde_json::to_vec(&config)?)?;
        assert!(Settings::load(&path).is_err());
        Ok(())
    }
}
