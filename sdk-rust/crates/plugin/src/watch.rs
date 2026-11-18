use std::future;
use std::time::Duration;

use tokio::sync::{mpsc, watch};
use tokio_stream::wrappers::ReceiverStream;
use tonic::Status;
use tonic::codegen::BoxStream;

use crate::proto::WatchResponse;

const HEARTBEAT: Duration = Duration::from_secs(30);

#[derive(Clone, Debug)]
pub struct Notifier {
    sender: watch::Sender<i64>,
}

impl Notifier {
    pub fn new() -> Self {
        let (sender, _) = watch::channel(0);
        Self { sender }
    }

    pub fn notify(&self, latest_seq: i64) {
        self.sender
            .send_modify(|current| *current = (*current).max(latest_seq));
    }

    pub fn status_changed(&self) {
        self.sender.send_modify(|_| {});
    }

    pub fn subscribe(&self) -> Signals {
        Signals {
            receiver: self.sender.subscribe(),
        }
    }
}

impl Default for Notifier {
    fn default() -> Self {
        Self::new()
    }
}

#[derive(Debug)]
pub struct Signals {
    receiver: watch::Receiver<i64>,
}

impl Signals {
    pub fn latest_seq(&mut self) -> i64 {
        *self.receiver.borrow_and_update()
    }

    pub async fn changed(&mut self) -> i64 {
        if self.receiver.changed().await.is_err() {
            return future::pending().await;
        }
        *self.receiver.borrow_and_update()
    }
}

pub(crate) fn stream(
    notifier: &Notifier,
    closing: watch::Receiver<bool>,
) -> BoxStream<WatchResponse> {
    let (tx, rx) = mpsc::channel(1);
    tokio::spawn(forward(notifier.subscribe(), closing, tx));
    Box::pin(ReceiverStream::new(rx))
}

async fn forward(
    mut signals: Signals,
    mut closing: watch::Receiver<bool>,
    tx: mpsc::Sender<Result<WatchResponse, Status>>,
) {
    loop {
        let permit = tokio::select! {
            biased;
            _ = closing.wait_for(|closing| *closing) => return,
            permit = tx.reserve() => permit,
        };
        let Ok(permit) = permit else {
            return;
        };
        permit.send(Ok(WatchResponse {
            latest_seq: signals.latest_seq(),
        }));
        tokio::select! {
            biased;
            _ = closing.wait_for(|closing| *closing) => return,
            () = tx.closed() => return,
            _ = signals.changed() => {}
            () = tokio::time::sleep(HEARTBEAT) => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use tokio::sync::watch;
    use tokio::task;
    use tokio::time::{Instant, sleep, timeout};
    use tokio_stream::StreamExt;
    use tonic::codegen::BoxStream;

    use super::{HEARTBEAT, Notifier, stream};
    use crate::proto::WatchResponse;

    const WAIT_LIMIT: Duration = Duration::from_secs(300);

    struct Watch {
        stream: BoxStream<WatchResponse>,
        closing: watch::Sender<bool>,
        t0: Instant,
    }

    impl Watch {
        fn open(notifier: &Notifier) -> Self {
            let (closing, closed) = watch::channel(false);
            let stream = stream(notifier, closed);
            Self {
                stream,
                closing,
                t0: Instant::now(),
            }
        }

        async fn next(&mut self) -> Option<i64> {
            timeout(WAIT_LIMIT, self.stream.next())
                .await
                .expect("the stream yields or ends in time")
                .map(|signal| signal.expect("an OK signal").latest_seq)
        }

        async fn signal(&mut self) -> i64 {
            self.next().await.expect("the stream is still open")
        }

        fn elapsed(&self) -> Duration {
            self.t0.elapsed()
        }
    }

    #[tokio::test(start_paused = true)]
    async fn the_first_signal_is_sent_at_once_with_the_latest_seq() {
        let notifier = Notifier::new();
        notifier.notify(7);
        let mut watch = Watch::open(&notifier);

        assert_eq!(watch.signal().await, 7);
        assert_eq!(watch.elapsed(), Duration::ZERO);
    }

    #[tokio::test(start_paused = true)]
    async fn notify_never_lowers_the_seq() {
        let notifier = Notifier::new();
        notifier.notify(9);
        notifier.notify(3);
        let mut watch = Watch::open(&notifier);

        assert_eq!(watch.signal().await, 9);
    }

    #[tokio::test(start_paused = true)]
    async fn signals_collapse_while_the_hub_reads_slowly() {
        let notifier = Notifier::new();
        let mut watch = Watch::open(&notifier);
        assert_eq!(watch.signal().await, 0);

        notifier.notify(8);
        notifier.notify(9);
        notifier.status_changed();

        assert_eq!(watch.signal().await, 9);
        let extra = timeout(HEARTBEAT - Duration::from_millis(1), watch.stream.next()).await;
        assert!(extra.is_err(), "unexpected extra signal {extra:?}");
    }

    #[tokio::test(start_paused = true)]
    async fn a_heartbeat_repeats_the_latest_seq_every_30_seconds() {
        let notifier = Notifier::new();
        notifier.notify(2);
        let mut watch = Watch::open(&notifier);
        assert_eq!(watch.signal().await, 2);

        assert_eq!(watch.signal().await, 2);
        assert_eq!(watch.elapsed(), Duration::from_secs(30));
        assert_eq!(watch.signal().await, 2);
        assert_eq!(watch.elapsed(), Duration::from_secs(60));
    }

    #[tokio::test(start_paused = true)]
    async fn a_signal_restarts_the_heartbeat() {
        let notifier = Notifier::new();
        let mut watch = Watch::open(&notifier);
        assert_eq!(watch.signal().await, 0);

        sleep(Duration::from_secs(20)).await;
        notifier.notify(5);

        assert_eq!(watch.signal().await, 5);
        assert_eq!(watch.elapsed(), Duration::from_secs(20));
        assert_eq!(watch.signal().await, 5);
        assert_eq!(watch.elapsed(), Duration::from_secs(50));
    }

    #[tokio::test(start_paused = true)]
    async fn status_changed_signals_with_the_same_seq() {
        let notifier = Notifier::new();
        notifier.notify(4);
        let mut watch = Watch::open(&notifier);
        assert_eq!(watch.signal().await, 4);

        notifier.status_changed();

        assert_eq!(watch.signal().await, 4);
        assert_eq!(watch.elapsed(), Duration::ZERO);
    }

    #[tokio::test(start_paused = true)]
    async fn the_stream_ends_when_the_server_closes() {
        let notifier = Notifier::new();
        let mut watch = Watch::open(&notifier);
        assert_eq!(watch.signal().await, 0);

        watch.closing.send_replace(true);

        assert_eq!(watch.next().await, None);
    }

    #[tokio::test(start_paused = true)]
    async fn the_task_stops_when_the_hub_drops_the_stream() {
        let notifier = Notifier::new();
        let mut watch = Watch::open(&notifier);
        assert_eq!(watch.signal().await, 0);
        assert_eq!(notifier.sender.receiver_count(), 1);

        let Watch {
            stream, closing, ..
        } = watch;
        drop(stream);
        assert!(!closing.is_closed(), "the server side is still open");

        for _ in 0..100 {
            if notifier.sender.receiver_count() == 0 {
                return;
            }
            task::yield_now().await;
        }
        panic!("the watch task still holds a subscription after 100 yields");
    }

    #[tokio::test(start_paused = true)]
    async fn notify_works_with_no_one_watching() {
        let notifier = Notifier::new();
        notifier.notify(3);
        notifier.status_changed();

        let mut signals = notifier.subscribe();
        assert_eq!(signals.latest_seq(), 3);

        notifier.notify(6);
        let changed = timeout(WAIT_LIMIT, signals.changed())
            .await
            .expect("changed resolves after notify");
        assert_eq!(changed, 6);
    }
}
