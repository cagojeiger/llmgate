use clap::ValueEnum;
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize, ValueEnum)]
#[serde(rename_all = "kebab-case")]
pub enum Profile {
    Embedding,
    Stt,
}

impl Profile {
    pub const ALL: [Self; 2] = [Self::Embedding, Self::Stt];
    pub fn id(self) -> &'static str {
        match self {
            Self::Embedding => "embedding",
            Self::Stt => "stt",
        }
    }
    pub fn model(self) -> &'static str {
        match self {
            Self::Embedding => "mlx-community/Qwen3-Embedding-0.6B-8bit",
            Self::Stt => "mlx-community/Qwen3-ASR-0.6B-4bit",
        }
    }
    pub fn revision(self) -> &'static str {
        match self {
            Self::Embedding => "407ad2329cd30702720aafe83f74a1ba30fdfbca",
            Self::Stt => "313d850181767edf09f00a9c289becca70e58cd0",
        }
    }
    pub fn served_name(self) -> &'static str {
        match self {
            Self::Embedding => "qwen3-embedding-0.6b",
            Self::Stt => "qwen3-asr-0.6b",
        }
    }
    pub fn modes(self) -> &'static [&'static str] {
        match self {
            Self::Stt => &["response", "sse"],
            Self::Embedding => &["response"],
        }
    }
    pub fn lock(self) -> &'static str {
        match self {
            Self::Embedding => include_str!("../profiles/mlx.lock"),
            Self::Stt => include_str!("../profiles/audio.lock"),
        }
    }
}
