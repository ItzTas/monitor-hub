use std::error::Error;
use std::path::Path;
use std::process::ExitCode;
use std::{env, fs};

use monitor_hub_plugin::migrate::{Migration, up};
use tokio_postgres::NoTls;

#[tokio::main(flavor = "current_thread")]
async fn main() -> ExitCode {
    match run().await {
        Ok(applied) => {
            for version in applied {
                println!("{version}");
            }
            ExitCode::SUCCESS
        }
        Err(err) => {
            eprintln!("migrate_fixture: {}", chain(err.as_ref()));
            ExitCode::FAILURE
        }
    }
}

async fn run() -> Result<Vec<i64>, Box<dyn Error>> {
    let args: Vec<String> = env::args().skip(1).collect();
    let [url, dir] = args.as_slice() else {
        return Err("usage: migrate_fixture <DATABASE_URL> <dir>".into());
    };
    let migrations = read(Path::new(dir))?;
    let (mut client, connection) = tokio_postgres::connect(url, NoTls).await?;
    tokio::spawn(async move {
        if let Err(err) = connection.await {
            eprintln!("migrate_fixture: connection: {err}");
        }
    });
    Ok(up(&mut client, &migrations).await?)
}

fn read(dir: &Path) -> Result<Vec<Migration>, Box<dyn Error>> {
    let mut migrations = Vec::new();
    for entry in fs::read_dir(dir)? {
        let path = entry?.path();
        if path.extension().is_none_or(|extension| extension != "sql") {
            continue;
        }
        let name = path
            .file_name()
            .and_then(|name| name.to_str())
            .ok_or_else(|| format!("{} is not a UTF-8 file name", path.display()))?;
        let version = name
            .split_once('_')
            .and_then(|(prefix, _)| prefix.parse().ok())
            .ok_or_else(|| format!("{name} does not start with a numeric version and '_'"))?;
        migrations.push(Migration {
            version,
            name: name.to_owned().leak(),
            sql: fs::read_to_string(&path)?.leak(),
        });
    }
    Ok(migrations)
}

fn chain(err: &dyn Error) -> String {
    let mut message = err.to_string();
    let mut source = err.source();
    while let Some(cause) = source {
        message.push_str(": ");
        message.push_str(&cause.to_string());
        source = cause.source();
    }
    message
}
