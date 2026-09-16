use relaygate_sdk::{
    AccessAction, AccessToken, AccessTokenSource, AccessTokenSourceError, Destination,
};
use relaygate_token_issuer::{Action, TokenIssuer};
use std::{sync::Arc, time::Duration};

pub fn source(
    issuer: TokenIssuer,
    destinations: Vec<Destination>,
    ttl: Duration,
) -> AccessTokenSource {
    let issuer = Arc::new(issuer);
    AccessTokenSource::dynamic(move |request| {
        let issuer = issuer.clone();
        let allowed =
            request.action == AccessAction::Dial && destinations.contains(&request.destination);
        async move {
            if !allowed {
                return Err(AccessTokenSourceError);
            }
            // Sign at every admission: idle time and token expiry need no external
            // refresh job, and no credential is cached on disk or sent by a worker.
            let token = issuer
                .issue_exact(Action::Dial, &request.destination, ttl)
                .map_err(|_| AccessTokenSourceError)?;
            AccessToken::new(token.into_string()).map_err(|_| AccessTokenSourceError)
        }
    })
}
