mod broker;
mod lifecycle;
mod logs;
mod profile;
mod relay;
mod runtime;
mod storage;

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
    Profiles,
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
    Install {
        #[arg(value_enum)]
        profile: Profile,
    },
    Start(lifecycle::StartOptions),
    #[command(hide = true)]
    Run(lifecycle::StartOptions),
    Status,
    Logs {
        #[arg(value_enum)]
        profile: Profile,
    },
    Down {
        #[arg(value_enum)]
        profile: Option<Profile>,
        #[arg(long)]
        all: bool,
        #[arg(long, requires = "all")]
        purge: bool,
    },
    Cache {
        #[command(subcommand)]
        command: CacheCommand,
    },
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
                "registered {} profile(s); API key stored in macOS Keychain",
                registration.profiles.len()
            );
        }
        Command::Install { profile } => {
            let _p = home.profile_lock(profile)?;
            let _c = home.cache_lock(false)?;
            runtime::install::install(&home, profile).await?;
            println!("{} installed", profile.id());
        }
        Command::Start(opts) => lifecycle::start(&home, opts).await?,
        Command::Run(opts) => {
            let p = opts.profile.context("run requires one profile")?;
            lifecycle::run(home.clone(), p, opts).await?;
        }
        Command::Status => {
            for p in Profile::ALL {
                match lifecycle::control(&home, p, "status").await {
                    Ok(state) => {
                        let mut value: serde_json::Value = serde_json::from_str(&state)?;
                        if let Ok(memory) = home
                            .read::<serde_json::Value>(&format!("control/{}-memory.json", p.id()))
                        {
                            value["memory"] = memory;
                        }
                        println!("{value}");
                    }
                    Err(_) => {
                        let last = home
                            .read::<lifecycle::State>(&format!("control/{}.json", p.id()))
                            .ok();
                        println!(
                            "{}",
                            serde_json::json!({"profile":p.id(),"supervisor":"not_running","installed":home.runtime(p).join("installed.json").exists(),"last_state":last})
                        );
                    }
                }
            }
        }
        Command::Logs { profile } => {
            let path = home.0.join(format!("logs/{}.log", profile.id()));
            if path.exists() {
                use std::io::{Seek, SeekFrom};
                let mut f = fs::File::open(path)?;
                let length = f.metadata()?.len();
                f.seek(SeekFrom::Start(length.saturating_sub(64 * 1024)))?;
                std::io::copy(&mut f, &mut std::io::stdout())?;
            }
        }
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
                println!("{} stopped and runtime removed", p.id());
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
