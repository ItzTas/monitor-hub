use monitor_hub_plugin::proto::WatchRequest;
use monitor_hub_plugin::proto::plugin_server::Plugin;
use tonic::{Code, Request};

struct Bare;

#[tonic::async_trait]
impl Plugin for Bare {}

#[tokio::test]
async fn a_plugin_without_the_notifier_answers_unimplemented() {
    let result = Bare.watch(Request::new(WatchRequest {})).await;
    let status = result.err().expect("watch must fail");
    assert_eq!(status.code(), Code::Unimplemented);
}
