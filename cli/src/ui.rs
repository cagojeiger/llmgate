use crate::{lifecycle, profile::Profile, storage::Home};
use serde_json::{Value, json};
use std::{
    fs,
    io::{Seek, SeekFrom},
    path::PathBuf,
};

pub fn command(home: &Home, args: &str) -> String {
    let default = PathBuf::from(std::env::var_os("HOME").unwrap_or_default()).join(".llmgate");
    if default.canonicalize().ok().as_ref() == Some(&home.0) {
        format!("llmgate-cli {args}")
    } else {
        let path = home.0.to_string_lossy().replace('\'', "'\\''");
        format!("llmgate-cli --home '{path}' {args}")
    }
}

pub fn runtime_label(value: &str) -> &str {
    match value {
        "starting" => "모델 준비 중",
        "ready" => "모델 준비 완료",
        "stopping" => "종료 중",
        "stopped" => "종료됨",
        "unhealthy" => "모델 재시작 중",
        "cleanup_failed" => "오류 · 로그 확인 필요",
        _ => value,
    }
}
pub fn publish_label(value: &str) -> &str {
    match value {
        "active" => "연결됨",
        "disabled" => "로컬 전용",
        "inactive" => "미연결",
        "connecting" | "retrying" | "reconnecting" => "연결 대기",
        _ => value,
    }
}

pub async fn status(home: &Home, machine: bool) -> anyhow::Result<()> {
    if !machine {
        println!("PROFILE    모델 상태                     RELAY");
    }
    for p in Profile::ALL {
        let value: Value = match lifecycle::control(home, p, "status").await {
            Ok(raw) => {
                let mut value: Value = serde_json::from_str(&raw)?;
                if let Ok(memory) = home.read::<Value>(&format!("control/{}-memory.json", p.id())) {
                    value["memory"] = memory;
                }
                value
            }
            Err(_) => json!({"profile":p.id(),"supervisor":"not_running",
                "installed":home.runtime(p).join("installed.json").exists(),
                "last_state":home.read::<lifecycle::State>(&format!("control/{}.json",p.id())).ok()}),
        };
        if machine {
            println!("{value}");
            continue;
        }
        if let Some(runtime) = value["runtime"].as_str() {
            let publish = value["publish"].as_str().unwrap_or("inactive");
            let label = if runtime == "ready" && publish == "active" {
                "서빙 가능"
            } else {
                runtime_label(runtime)
            };
            println!("{:<10} {:<24} {}", p.id(), label, publish_label(publish));
            if let Some(error) = value["error"].as_str() {
                println!("  오류: {error}");
            }
            if let (Some(total), Some(target), Some(average), Some(sampled)) = (
                value["memory"]["total_bytes"].as_u64(),
                value["memory"]["target_bytes"].as_u64(),
                value["memory"]["average_bytes"].as_u64(),
                value["memory"]["sampled_at"].as_f64(),
            ) && (crate::broker::now() as f64 - sampled).abs() <= 5.0
            {
                println!(
                    "  관리 모델 합산 메모리: 현재 {:.2} GiB · 최근 최대 60초 평균 {:.2} GiB / 목표 {:.2} GiB",
                    total as f64 / 1073741824.0,
                    average as f64 / 1073741824.0,
                    target as f64 / 1073741824.0
                );
            }
        } else {
            let label = if home.profile_lock(p).is_err() {
                "감독 응답 없음"
            } else if home.engine_lock(p).is_err() {
                "모델 종료 대기"
            } else if value["installed"] == true {
                "정지 · 설치됨"
            } else {
                "정지 · 미설치"
            };
            println!("{:<10} {:<24} —", p.id(), label);
            if let Some(error) = value["last_state"]["error"].as_str() {
                println!(
                    "  마지막 오류: {error}\n  확인: {}",
                    command(home, &format!("logs {}", p.id()))
                );
            }
        }
    }
    if !machine {
        println!(
            "다음: {}",
            if home.0.join("registration.json").exists() {
                command(home, "start --all")
            } else {
                command(home, "register --url <서버 URL> --profiles embedding stt")
            }
        );
    }
    Ok(())
}

pub fn logs(home: &Home, p: Profile) -> anyhow::Result<()> {
    let mut found = false;
    for (name, label) in [
        (format!("{}-supervisor.log", p.id()), "시작·인증·감독"),
        (format!("{}.log", p.id()), "모델"),
    ] {
        let path = home.0.join("logs").join(name);
        if path.exists() {
            found = true;
            println!("--- {label}: {} (최근 최대 64 KiB) ---", path.display());
            let mut file = fs::File::open(path)?;
            let size = file.metadata()?.len();
            file.seek(SeekFrom::Start(size.saturating_sub(64 * 1024)))?;
            std::io::copy(&mut file, &mut std::io::stdout())?;
            println!();
        }
    }
    if !found {
        println!(
            "{} 로그가 아직 없습니다. 먼저 {}로 등록·실행 상태를 확인하세요.",
            p.id(),
            command(home, "status")
        );
    }
    Ok(())
}
