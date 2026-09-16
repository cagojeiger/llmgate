use crate::{
    broker::Registration,
    lifecycle::{self, StartOptions, State, control},
    profile::Profile,
    runtime::{install, ownership},
    storage::Home,
    ui,
};
use anyhow::{Context, ensure};
use std::{process::Child, time::Duration};
use tokio::time::{Instant, sleep};

pub async fn start(home: &Home, opts: StartOptions) -> anyhow::Result<()> {
    ensure!(
        opts.all != opts.profile.is_some(),
        "choose one profile or --all"
    );
    ensure!(
        !opts.all || (!opts.foreground && opts.port.is_none()),
        "--all requires background mode and automatic ports"
    );
    let registered = if opts.local_only {
        None
    } else {
        let register = ui::command(home, "register --url <서버 URL> --profiles embedding stt");
        ensure!(
            home.0.join("registration.json").is_file(),
            "등록 정보가 없습니다. 먼저 {register}"
        );
        Some(
            home.read::<Registration>("registration.json")
                .with_context(|| {
                    format!("등록 설정을 읽지 못했습니다. 다시 등록하세요: {register}")
                })?,
        )
    };
    let profiles = if opts.all {
        registered
            .as_ref()
            .map(|r| r.profiles.clone())
            .unwrap_or_else(|| Profile::ALL.to_vec())
    } else {
        vec![opts.profile.context("missing profile")?]
    };
    ensure!(
        !profiles.is_empty(),
        "등록된 profile이 없습니다. register로 사용할 모델을 등록하세요"
    );
    if let Some(r) = &registered {
        ensure!(
            profiles.iter().all(|p| r.profiles.contains(p)),
            "등록하지 않은 profile입니다. register --profiles로 사용할 모델을 등록하세요"
        );
    }
    for p in profiles {
        if control(home, p, "status").await.is_ok() {
            ensure!(
                !opts.foreground,
                "이미 실행 중입니다. foreground로 전환하려면 {} 후 다시 시작하세요",
                ui::command(home, &format!("down {}", p.id()))
            );
            wait_ready(home, p, &opts, None).await?;
            continue;
        }
        let profile_lock = home.profile_lock(p)?;
        let engine_lock = ownership::idle(home, p).await?;
        let cache_lock = home.cache_lock(false)?;
        eprintln!(
            "{}: 실행 환경·모델 설치 확인 중 (첫 실행에는 다운로드가 필요합니다)",
            p.id()
        );
        install::install(home, p).await.with_context(|| {
            format!(
                "{} 설치 실패. 네트워크·디스크 공간을 확인하고 같은 start 명령을 다시 실행하세요",
                p.id()
            )
        })?;
        drop(cache_lock);
        drop(engine_lock);
        drop(profile_lock);
        if opts.foreground {
            let stop = tokio_util::sync::CancellationToken::new();
            let supervisor = lifecycle::run(home.clone(), p, opts.clone(), stop.clone());
            tokio::pin!(supervisor);
            return tokio::select! {
                result = &mut supervisor => result,
                ready = wait_ready(home, p, &opts, None) => {
                    if let Err(error) = ready {
                        stop.cancel();
                        let _ = supervisor.await;
                        Err(error)
                    } else {
                        supervisor.await
                    }
                }
            };
        }
        let mut command = std::process::Command::new(std::env::current_exe()?);
        command.arg("--home").arg(&home.0).args([
            "run",
            p.id(),
            "--max-connections",
            &opts.max_connections.to_string(),
            "--connection-timeout",
            &opts.connection_timeout.to_string(),
        ]);
        if opts.local_only {
            command.arg("--local-only");
        }
        if let Some(port) = opts.port {
            command.args(["--port", &port.to_string()]);
        }
        use std::os::unix::process::CommandExt;
        let mut child = command
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .process_group(0)
            .spawn()?;
        wait_ready(home, p, &opts, Some(&mut child)).await?;
    }
    Ok(())
}

async fn wait_ready(
    home: &Home,
    p: Profile,
    opts: &StartOptions,
    mut child: Option<&mut Child>,
) -> anyhow::Result<()> {
    let began = Instant::now();
    let mut previous = String::new();
    let mut seen = false;
    let mut last_notice = Instant::now();
    loop {
        if let Some(child) = child.as_mut() {
            ensure!(
                child.try_wait()?.is_none(),
                "시작 프로세스가 종료됐습니다. {}",
                ui::command(home, &format!("logs {}", p.id()))
            );
        }
        if let Ok(raw) = control(home, p, "status").await {
            seen = true;
            let state: State = serde_json::from_str(&raw)?;
            if state.runtime == "ready"
                && (state.publish == "active" || (opts.local_only && state.publish == "disabled"))
            {
                if state.publish == "active" {
                    println!("{}: 서빙 준비 완료 (모델 준비 · Relay 연결 완료)", p.id());
                } else {
                    println!("{}: 로컬 모델 준비 완료 (Relay 공개 안 함)", p.id());
                }
                return Ok(());
            }
            ensure!(
                opts.local_only || state.publish != "disabled",
                "로컬 전용으로 실행 중입니다. {} 후 --local-only 없이 start를 실행하세요",
                ui::command(home, &format!("down {}", p.id()))
            );
            ensure!(
                !matches!(state.runtime.as_str(), "stopped" | "cleanup_failed"),
                "모델이 종료됐습니다. {}",
                ui::command(home, &format!("logs {}", p.id()))
            );
            let phase = format!(
                "{} · Relay {}",
                ui::runtime_label(&state.runtime),
                ui::publish_label(&state.publish)
            );
            if previous != phase || last_notice.elapsed() >= Duration::from_secs(10) {
                eprintln!("{}: {} ({}초)", p.id(), phase, began.elapsed().as_secs());
                previous = phase;
                last_notice = Instant::now();
            }
        } else if seen && home.profile_lock(p).is_ok() {
            anyhow::bail!(
                "감독 프로세스가 종료됐습니다. {}",
                ui::command(home, &format!("logs {}", p.id()))
            );
        }
        ensure!(
            began.elapsed() < Duration::from_secs(opts.wait_timeout as u64),
            "{}초 안에 준비되지 않았습니다. {}. {} / {}",
            opts.wait_timeout,
            if opts.foreground {
                "foreground 워커를 종료합니다"
            } else {
                "백그라운드 워커는 계속 실행·재연결합니다"
            },
            ui::command(home, "status"),
            ui::command(home, &format!("logs {}", p.id()))
        );
        tokio::select! {
            _ = tokio::signal::ctrl_c() => anyhow::bail!("대기를 취소했습니다. {}. {}", if opts.foreground {"foreground 워커를 종료합니다"} else {"백그라운드 워커는 계속 실행합니다"}, ui::command(home, "status")),
            _ = sleep(Duration::from_millis(250)) => {}
        }
    }
}
