mod broker;
mod lifecycle;
mod logs;
mod profile;
mod relay;
mod runtime;
mod startup;
mod storage;
mod ui;

use anyhow::{Context, ensure};
use clap::{Parser, Subcommand};
use profile::Profile;
use std::{fs, io::Read, path::PathBuf};
use storage::Home;

#[derive(Parser)]
#[command(version, about = "MacBook MLX serving agent for LLMGate")]
struct Cli {
    #[arg(long, global = true)]
    home: Option<PathBuf>,
    #[command(subcommand)]
    command: Command,
}
#[derive(Subcommand)]
enum Command {
    /// 사용 가능한 모델과 입력 모드를 표시합니다.
    Profiles,
    /// API 키로 모델 사용 권한을 확인하고 이 Mac에 등록합니다.
    Register {
        #[arg(long)]
        url: String,
        #[arg(long,value_enum,required=true,num_args=1..)]
        profiles: Vec<Profile>,
        #[arg(long)]
        key_stdin: bool,
        #[arg(long)]
        ca_file: Option<PathBuf>,
        #[arg(long)]
        allow_loopback_http: bool,
    },
    /// 모델과 전용 Python 환경을 설치합니다.
    Install {
        #[arg(value_enum)]
        profile: Profile,
    },
    /// 설치 후 모델 준비·Relay 공개까지 기다립니다.
    Start(lifecycle::StartOptions),
    #[command(hide = true)]
    Run(lifecycle::StartOptions),
    /// 모델·Relay 연결 상태를 확인합니다.
    Status {
        /// 자동화용 NDJSON (profile마다 한 줄).
        #[arg(long)]
        json: bool,
    },
    /// 시작·인증 오류와 모델 로그의 최근 내용을 표시합니다.
    Logs {
        #[arg(value_enum)]
        profile: Profile,
    },
    /// 워커를 종료하고 전용 실행 환경을 삭제합니다 (캐시는 유지).
    Down {
        #[arg(value_enum)]
        profile: Option<Profile>,
        #[arg(long)]
        all: bool,
        #[arg(long, requires = "all")]
        purge: bool,
    },
    /// 캐시 용량 확인 또는 실행 중이지 않은 캐시 삭제.
    Cache {
        #[command(subcommand)]
        command: CacheCommand,
    },
    /// 모든 워커를 종료하고 이 Mac의 키·등록 정보를 삭제합니다.
    Unregister,
}
#[derive(Subcommand)]
enum CacheCommand {
    List,
    Clean,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let cli = Cli::parse();
    let home = Home::open(cli.home)?;
    match cli.command {
        Command::Profiles => {
            for p in Profile::ALL {
                println!(
                    "{}\t{}\t{}\t{}",
                    p.id(),
                    p.model(),
                    p.revision(),
                    p.modes().join(",")
                );
            }
        }
        Command::Register {
            url,
            profiles,
            key_stdin,
            ca_file,
            allow_loopback_http,
        } => {
            let registration = broker::Registration {
                url,
                profiles,
                ca_file: ca_file.map(|p| p.canonicalize()).transpose()?,
                allow_loopback_http,
            };
            let client = broker::client(&registration)?;
            let key = if key_stdin {
                let mut s = String::new();
                std::io::stdin().take(4097).read_to_string(&mut s)?;
                s.trim().to_owned()
            } else {
                rpassword::prompt_password("LLMGate API key: ")?
            };
            ensure!(
                !key.is_empty() && key.len() <= 4096,
                "invalid API key length"
            );
            for p in &registration.profiles {
                broker::grant(&client, &registration, &key, *p).await?;
            }
            broker::save_key(&home, &key)?;
            home.write("registration.json", &registration)?;
            println!(
                "{}개 profile 등록 완료. API 키는 macOS Keychain에 저장했습니다. 모델은 아직 시작하지 않았습니다.",
                registration.profiles.len()
            );
            println!("다음: {}", ui::command(&home, "start --all"));
        }
        Command::Install { profile } => {
            let _p = home.profile_lock(profile)?;
            let _engine = runtime::ownership::idle(&home, profile).await?;
            let _c = home.cache_lock(false)?;
            runtime::install::install(&home, profile).await?;
            println!("{} installed", profile.id());
        }
        Command::Start(opts) => startup::start(&home, opts).await?,
        Command::Run(opts) => {
            let p = opts.profile.context("run requires one profile")?;
            lifecycle::run(
                home.clone(),
                p,
                opts,
                tokio_util::sync::CancellationToken::new(),
            )
            .await?;
        }
        Command::Status { json } => ui::status(&home, json).await?,
        Command::Logs { profile } => ui::logs(&home, profile)?,
        Command::Down {
            profile,
            all,
            purge,
        } => {
            ensure!(all != profile.is_some(), "choose one profile or --all");
            for p in if all {
                Profile::ALL.to_vec()
            } else {
                vec![profile.context("missing profile")?]
            } {
                lifecycle::stop(&home, p).await?;
                println!(
                    "{} 종료·실행 환경 삭제 완료. 캐시는 유지되며 다음 start에서 실행 환경을 다시 설치합니다.",
                    p.id()
                );
            }
            if purge {
                clean_cache(&home)?;
            }
        }
        Command::Cache { command } => match command {
            CacheCommand::List => {
                for kind in ["packages", "models"] {
                    println!(
                        "{kind}\t{} bytes",
                        storage::bytes(&home.0.join("cache").join(kind))?
                    );
                }
            }
            CacheCommand::Clean => clean_cache(&home)?,
        },
        Command::Unregister => {
            for p in Profile::ALL {
                lifecycle::stop(&home, p).await?;
            }
            if home.0.join("registration.json").exists() {
                broker::delete_key(&home)?;
                fs::remove_file(home.0.join("registration.json"))?;
            }
            println!("local credentials removed; model cache and logs retained");
        }
    }
    Ok(())
}
fn clean_cache(home: &Home) -> anyhow::Result<()> {
    let _lock = home.cache_lock(true)?;
    for kind in ["packages", "models"] {
        let path = home.0.join("cache").join(kind);
        fs::remove_dir_all(&path)?;
        fs::create_dir_all(path)?;
    }
    println!("cache removed; configuration and credentials retained");
    Ok(())
}
