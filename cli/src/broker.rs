use crate::{profile::Profile, storage::Home};
use anyhow::{Context, ensure};
use relaygate_sdk::{AccessAction, AccessToken, AccessTokenSource, AccessTokenSourceError};
use serde::{Deserialize, Serialize};
use std::{
    path::PathBuf,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio::sync::Mutex;

#[derive(Clone, Serialize, Deserialize)]
pub struct Registration {
    pub url: String,
    pub profiles: Vec<Profile>,
    pub ca_file: Option<PathBuf>,
    pub allow_loopback_http: bool,
}
#[derive(Clone, Deserialize)]
pub struct Grant {
    pub protocol_version: u32,
    pub profile: String,
    pub profile_version: String,
    pub destination: String,
    pub gateway_endpoint: String,
    pub access_token: String,
    pub expires_at: u64,
}

pub fn now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}
pub fn key_account(home: &Home) -> String {
    home.0.to_string_lossy().into_owned()
}
pub fn load_key(home: &Home) -> anyhow::Result<String> {
    Ok(String::from_utf8(
        security_framework::passwords::get_generic_password("llmgate-cli", &key_account(home))
            .context("read API key from macOS Keychain; run register")?,
    )?)
}
pub fn save_key(home: &Home, key: &str) -> anyhow::Result<()> {
    security_framework::passwords::set_generic_password(
        "llmgate-cli",
        &key_account(home),
        key.as_bytes(),
    )?;
    Ok(())
}
pub fn delete_key(home: &Home) -> anyhow::Result<()> {
    security_framework::passwords::delete_generic_password("llmgate-cli", &key_account(home))?;
    Ok(())
}

pub fn client(r: &Registration) -> anyhow::Result<reqwest::Client> {
    let url = reqwest::Url::parse(&r.url)?;
    let local = matches!(url.host_str(), Some("localhost" | "127.0.0.1" | "[::1]"));
    ensure!(
        url.scheme() == "https" || (r.allow_loopback_http && local && url.scheme() == "http"),
        "registration requires HTTPS; explicit loopback HTTP is for local tests only"
    );
    ensure!(
        url.username().is_empty()
            && url.password().is_none()
            && url.query().is_none()
            && url.fragment().is_none(),
        "URL must not contain credentials, query, or fragment"
    );
    let mut b = reqwest::Client::builder()
        .timeout(Duration::from_secs(10))
        .redirect(reqwest::redirect::Policy::none());
    if let Some(path) = &r.ca_file {
        b = b.add_root_certificate(reqwest::Certificate::from_pem(&std::fs::read(path)?)?);
    }
    Ok(b.build()?)
}
pub async fn grant(
    client: &reqwest::Client,
    r: &Registration,
    key: &str,
    p: Profile,
) -> anyhow::Result<Grant> {
    let mut response = client
        .post(format!("{}/v1/workers/token", r.url.trim_end_matches('/')))
        .bearer_auth(key)
        .json(&serde_json::json!({"protocol_version":1,"profile":p.id(),"profile_version":"1"}))
        .send()
        .await?;
    ensure!(
        response.status().is_success(),
        "token broker denied request (HTTP {})",
        response.status().as_u16()
    );
    let mut body = Vec::new();
    while let Some(chunk) = response.chunk().await? {
        ensure!(
            body.len() + chunk.len() <= 32 * 1024,
            "token response too large"
        );
        body.extend_from_slice(&chunk);
    }
    let g: Grant = serde_json::from_slice(&body)?;
    ensure!(
        g.protocol_version == 1
            && g.profile == p.id()
            && g.profile_version == "1"
            && g.expires_at > now() + 15,
        "unsupported or expired profile grant"
    );
    ensure!(
        g.gateway_endpoint.starts_with("tls://"),
        "Gateway endpoint must use TLS"
    );
    let _: relaygate_sdk::Destination = g.destination.parse()?;
    Ok(g)
}
pub fn source(
    client: reqwest::Client,
    r: Registration,
    key: String,
    p: Profile,
    initial: Grant,
) -> AccessTokenSource {
    let cache = Arc::new(Mutex::new(initial));
    AccessTokenSource::dynamic(move |request| {
        let (cache, client, r, key) = (cache.clone(), client.clone(), r.clone(), key.clone());
        async move {
            if request.action != AccessAction::Publish {
                return Err(AccessTokenSourceError);
            }
            let mut c = cache.lock().await;
            if c.destination != request.destination.to_string() {
                return Err(AccessTokenSourceError);
            }
            if c.expires_at <= now() + 30 {
                let next = grant(&client, &r, &key, p)
                    .await
                    .map_err(|_| AccessTokenSourceError)?;
                if next.destination != c.destination || next.gateway_endpoint != c.gateway_endpoint
                {
                    return Err(AccessTokenSourceError);
                }
                *c = next;
            }
            AccessToken::new(c.access_token.clone()).map_err(|_| AccessTokenSourceError)
        }
    })
}
