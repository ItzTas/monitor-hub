use tonic::{Request, Response, Status};

use crate::proto::{
    CheckRequest, CheckResponse, DescribeRequest, DescribeResponse, EventsRequest, EventsResponse,
    StatusRequest, StatusResponse,
};

/// Implement this for a monitor and pass it to [`serve`](crate::serve), which answers `Watch`.
#[tonic::async_trait]
pub trait Monitor: Send + Sync + 'static {
    async fn describe(
        &self,
        request: Request<DescribeRequest>,
    ) -> Result<Response<DescribeResponse>, Status>;

    async fn status(
        &self,
        request: Request<StatusRequest>,
    ) -> Result<Response<StatusResponse>, Status>;

    async fn check(
        &self,
        request: Request<CheckRequest>,
    ) -> Result<Response<CheckResponse>, Status>;

    async fn events(
        &self,
        request: Request<EventsRequest>,
    ) -> Result<Response<EventsResponse>, Status>;
}
