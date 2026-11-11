use std::env;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::process::ExitCode;

fn main() -> ExitCode {
    let sdk = Path::new(env!("CARGO_MANIFEST_DIR")).join("../..");
    let out = env::args_os()
        .nth(1)
        .map_or_else(|| sdk.join("crates/plugin/src/generated"), PathBuf::from);
    if let Err(err) = generate(&sdk, &out) {
        eprintln!("codegen: {err}");
        return ExitCode::FAILURE;
    }
    ExitCode::SUCCESS
}

fn generate(sdk: &Path, out: &Path) -> io::Result<()> {
    let contract = sdk.join("../proto").canonicalize()?;
    let controller = sdk.join("crates/plugin/proto").canonicalize()?;
    fs::create_dir_all(out)?;
    tonic_prost_build::configure()
        .build_client(false)
        .emit_rerun_if_changed(false)
        .out_dir(out)
        .compile_protos(
            &[
                contract.join("monitorhub/plugin/v1/plugin.proto"),
                controller.join("controller.proto"),
            ],
            &[contract, controller],
        )
}
