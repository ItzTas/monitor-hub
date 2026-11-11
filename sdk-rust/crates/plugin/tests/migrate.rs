use monitor_hub_plugin::migrate::{MigrateError, Migration, up};
use monitor_hub_plugin_testkit::database;
use tokio_postgres::{Client, NoTls};

const A: Migration = Migration {
    version: 1,
    name: "00001_a.sql",
    sql: "-- +goose Up\nCREATE TABLE a (id integer);\n\n-- +goose Down\nDROP TABLE a;\n",
};

const B: Migration = Migration {
    version: 2,
    name: "00002_b.sql",
    sql: "-- +goose Up\nCREATE TABLE b (\n    id integer\n);\n-- +goose Down\nDROP TABLE b;\n",
};

const C: Migration = Migration {
    version: 3,
    name: "00003_c.sql",
    sql: "-- +goose Up\nCREATE TABLE c (id integer);\n",
};

const SLOW_A: Migration = Migration {
    version: 1,
    name: "00001_a.sql",
    sql: "-- +goose Up\nSELECT pg_sleep(2);\nCREATE TABLE a (id integer);\n",
};

const BOTH: &[Migration] = &[SLOW_A, B];

async fn connect(url: &str) -> Client {
    let (client, connection) = tokio_postgres::connect(url, NoTls)
        .await
        .expect("connect to the test database");
    tokio::spawn(async move {
        if let Err(err) = connection.await {
            eprintln!("test connection failed: {err}");
        }
    });
    client
}

async fn history(client: &Client) -> Vec<(i64, bool)> {
    client
        .query(
            "SELECT version_id, is_applied FROM goose_db_version ORDER BY id",
            &[],
        )
        .await
        .expect("read goose_db_version")
        .iter()
        .map(|row| (row.get(0), row.get(1)))
        .collect()
}

async fn exists(client: &Client, relation: &str) -> bool {
    client
        .query_one("SELECT to_regclass($1) IS NOT NULL", &[&relation])
        .await
        .expect("look up a relation")
        .get(0)
}

#[tokio::test]
async fn fresh_database_gets_the_goose_table_and_every_version() {
    let Some(db) = database().await else { return };
    let mut client = connect(&db.url).await;

    let applied = up(&mut client, &[B, A]).await.expect("migrate");

    assert_eq!(applied, [1, 2]);
    assert_eq!(history(&client).await, [(0, true), (1, true), (2, true)]);
    assert!(exists(&client, "a").await);
    assert!(exists(&client, "b").await);
}

#[tokio::test]
async fn second_run_applies_nothing() {
    let Some(db) = database().await else { return };
    let mut client = connect(&db.url).await;
    up(&mut client, &[A, B]).await.expect("first run");

    let applied = up(&mut client, &[A, B]).await.expect("second run");

    assert_eq!(applied, Vec::<i64>::new());
    assert_eq!(history(&client).await, [(0, true), (1, true), (2, true)]);
}

#[tokio::test]
async fn only_missing_versions_are_applied() {
    let Some(db) = database().await else { return };
    let mut client = connect(&db.url).await;
    up(&mut client, &[A])
        .await
        .expect("apply the first version");

    let applied = up(&mut client, &[A, B]).await.expect("apply the rest");

    assert_eq!(applied, [2]);
    assert_eq!(history(&client).await, [(0, true), (1, true), (2, true)]);
}

#[tokio::test]
async fn failed_migration_rolls_back_and_records_nothing() {
    let Some(db) = database().await else { return };
    let mut client = connect(&db.url).await;
    let broken = Migration {
        version: 2,
        name: "00002_broken.sql",
        sql: "-- +goose Up\nCREATE TABLE partial (id integer);\nSELECT * FROM no_such_table;\n",
    };

    let err = up(&mut client, &[A, broken]).await.expect_err("must fail");

    let MigrateError::Failed { migration, source } = err else {
        panic!("expected Failed, got {err:?}");
    };
    assert_eq!(migration, "00002_broken.sql");
    let message = source.as_db_error().map(|db| db.message().to_owned());
    assert!(
        message
            .as_deref()
            .is_some_and(|m| m.contains("no_such_table")),
        "{source:?}"
    );
    assert_eq!(history(&client).await, [(0, true), (1, true)]);
    assert!(exists(&client, "a").await);
    assert!(!exists(&client, "partial").await);
}

#[tokio::test]
async fn unsupported_annotation_is_rejected_before_anything_runs() {
    let Some(db) = database().await else { return };
    let mut client = connect(&db.url).await;

    for annotation in ["NO TRANSACTION", "ENVSUB ON", "ENVSUB OFF"] {
        let sql = format!("-- +goose Up\n-- +goose {annotation}\nCREATE TABLE b (id integer);\n");
        let b = Migration {
            version: 2,
            name: "00002_b.sql",
            sql: sql.leak(),
        };

        let err = up(&mut client, &[A, b]).await.expect_err("must fail");

        let MigrateError::Unsupported {
            migration,
            annotation: found,
        } = err
        else {
            panic!("expected Unsupported, got {err:?}");
        };
        assert_eq!(
            (migration.as_str(), found.as_str()),
            ("00002_b.sql", annotation)
        );
        assert!(!exists(&client, "goose_db_version").await);
        assert!(!exists(&client, "a").await);
    }
}

#[tokio::test]
async fn invalid_migrations_are_rejected_before_anything_runs() {
    let Some(db) = database().await else { return };
    let mut client = connect(&db.url).await;
    let cases = [
        (
            vec![
                A,
                Migration {
                    version: 1,
                    name: "00001_again.sql",
                    ..B
                },
            ],
            "00001_again.sql",
        ),
        (vec![A, Migration { version: 0, ..B }], "00002_b.sql"),
        (vec![Migration { version: -1, ..A }], "00001_a.sql"),
        (
            vec![
                A,
                Migration {
                    sql: "CREATE TABLE b (id integer);\n",
                    ..B
                },
            ],
            "00002_b.sql",
        ),
    ];

    for (migrations, name) in cases {
        let err = up(&mut client, &migrations).await.expect_err("must fail");

        let MigrateError::Invalid { migration, .. } = err else {
            panic!("expected Invalid, got {err:?}");
        };
        assert_eq!(migration, name);
        assert!(!exists(&client, "goose_db_version").await);
    }
}

#[tokio::test]
async fn out_of_order_version_is_rejected() {
    let Some(db) = database().await else { return };
    let mut client = connect(&db.url).await;
    up(&mut client, &[A, C]).await.expect("apply 1 and 3");

    let err = up(&mut client, &[A, B, C]).await.expect_err("must fail");

    assert!(
        matches!(
            err,
            MigrateError::OutOfOrder {
                missing: 2,
                current: 3
            }
        ),
        "{err:?}"
    );
    assert!(!exists(&client, "b").await);
    assert_eq!(history(&client).await, [(0, true), (1, true), (3, true)]);
}

#[tokio::test]
async fn versions_the_list_does_not_know_are_left_alone() {
    let Some(db) = database().await else { return };
    let mut client = connect(&db.url).await;
    up(&mut client, &[A, B, C]).await.expect("newer run");

    let applied = up(&mut client, &[A, B]).await.expect("older run");

    assert_eq!(applied, Vec::<i64>::new());
    assert_eq!(
        history(&client).await,
        [(0, true), (1, true), (2, true), (3, true)]
    );
}

#[tokio::test]
async fn empty_list_creates_the_goose_table_with_version_zero() {
    let Some(db) = database().await else { return };
    let mut client = connect(&db.url).await;

    let applied = up(&mut client, &[]).await.expect("migrate");

    assert_eq!(applied, Vec::<i64>::new());
    assert_eq!(history(&client).await, [(0, true)]);
}

#[tokio::test]
async fn history_without_version_zero_is_rejected() {
    let Some(db) = database().await else { return };
    let mut client = connect(&db.url).await;
    up(&mut client, &[]).await.expect("create the goose table");
    client
        .batch_execute("DELETE FROM goose_db_version")
        .await
        .expect("empty the history");

    let err = up(&mut client, &[A]).await.expect_err("must fail");

    assert!(matches!(err, MigrateError::MissingZeroVersion), "{err:?}");
    assert!(!exists(&client, "a").await);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn concurrent_runs_apply_each_version_once() {
    let Some(db) = database().await else { return };
    let mut first = connect(&db.url).await;
    let mut second = connect(&db.url).await;
    up(&mut first, &[]).await.expect("create the goose table");

    let (left, right) = tokio::join!(
        tokio::spawn(async move { up(&mut first, BOTH).await }),
        tokio::spawn(async move { up(&mut second, BOTH).await }),
    );

    let mut applied = left.expect("first task").expect("first run");
    applied.extend(right.expect("second task").expect("second run"));
    applied.sort_unstable();
    assert_eq!(applied, [1, 2]);
    let client = connect(&db.url).await;
    assert_eq!(history(&client).await, [(0, true), (1, true), (2, true)]);
}

#[tokio::test]
async fn statement_blocks_keep_semicolons_together() {
    let Some(db) = database().await else { return };
    let mut client = connect(&db.url).await;
    let function = Migration {
        version: 1,
        name: "00001_function.sql",
        sql: "-- +goose Up\n\
              -- +goose StatementBegin\n\
              CREATE FUNCTION add_one(n integer) RETURNS integer AS $$\n\
              BEGIN\n\
              \x20   RETURN n + 1;\n\
              END;\n\
              $$ LANGUAGE plpgsql;\n\
              -- +goose StatementEnd\n\
              -- +goose Down\n\
              DROP FUNCTION add_one(integer);\n",
    };

    up(&mut client, &[function]).await.expect("migrate");

    let row = client
        .query_one("SELECT add_one(41)", &[])
        .await
        .expect("call the function");
    assert_eq!(row.get::<_, i32>(0), 42);
}
