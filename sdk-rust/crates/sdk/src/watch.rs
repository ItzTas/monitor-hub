use std::future;
use std::time::Duration;

use tokio::sync::{mpsc, watch};
use tokio_stream::wrappers::ReceiverStream;
use tonic::Status;
use tonic::codegen::BoxStream;

use crate::proto::WatchResponse;

const HEARTBEAT: Duration = Duration::from_secs(30);

/// How a monitor signals the hub through `Watch`; keep a clone and pass one to
/// [`serve`](crate::serve).
#[derive(Clone, Debug)]
pub struct Notifier {
    sender: watch::Sender<i64>,
}

impl Notifier {
    pub fn new() -> Self {
        let (sender, _) = watch::channel(0);
        Self { sender }
    }

    /// Call after recording events, with the seq of the newest one.
    pub fn events_recorded(&self, latest_seq: i64) {
        self.sender
            .send_modify(|current| *current = (*current).max(latest_seq));
    }

    /// Call for a one-off change outside a cycle, such as a pause, a failure or a return to normal.
    pub fn status_changed(&self) {
        self.sender.send_modify(|_| {});
    }

    /// Call at the start of a cycle: signals now and again when the cycle ends, however it ends.
    ///
    /// `let _ = notifier.status_scope();` drops it at once and signals twice; bind it to a name
    /// such as `_cycle`.
    pub fn status_scope(&self) -> StatusScope {
        self.status_changed();
        StatusScope {
            notifier: self.clone(),
        }
    }

    /// Listens to this notifier's signals the way `Watch` does; useful in tests.
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

/// Keep it alive for the whole cycle; dropping it, by `?` or a panic too, signals the hub again.
///
/// `let _ = notifier.status_scope();` drops it at once and signals twice; bind it to a name such
/// as `_cycle`.
#[must_use = "bind the scope to a variable such as _cycle; it signals again when dropped"]
#[derive(Debug)]
pub struct StatusScope {
    notifier: Notifier,
}

impl Drop for StatusScope {
    fn drop(&mut self) {
        self.notifier.status_changed();
    }
}

/// One listener's view of a [`Notifier`]: the latest seq, and a wait for the next signal.
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
    use std::panic::{self, AssertUnwindSafe};
    use std::time::Duration;

    use tokio::sync::watch;
    use tokio::task;
    use tokio::time::{Instant, sleep, timeout};
    use tokio_stream::StreamExt;
    use tonic::codegen::BoxStream;

    use super::{HEARTBEAT, Notifier, Signals, stream};
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

    async fn settle() {
        for _ in 0..10 {
            task::yield_now().await;
        }
    }

    #[tokio::test(start_paused = true)]
    async fn the_first_signal_is_sent_at_once_with_the_latest_seq() {
        let notifier = Notifier::new();
        notifier.events_recorded(7);
        let mut watch = Watch::open(&notifier);

        assert_eq!(watch.signal().await, 7);
        assert_eq!(watch.elapsed(), Duration::ZERO);
    }

    #[tokio::test(start_paused = true)]
    async fn events_recorded_never_lowers_the_seq() {
        let notifier = Notifier::new();
        notifier.events_recorded(9);
        notifier.events_recorded(3);
        let mut watch = Watch::open(&notifier);

        assert_eq!(watch.signal().await, 9);
    }

    #[tokio::test(start_paused = true)]
    async fn signals_collapse_while_the_hub_reads_slowly() {
        let notifier = Notifier::new();
        let mut watch = Watch::open(&notifier);
        assert_eq!(watch.signal().await, 0);

        notifier.events_recorded(8);
        notifier.events_recorded(9);
        notifier.status_changed();

        assert_eq!(watch.signal().await, 9);
        let extra = timeout(HEARTBEAT - Duration::from_millis(1), watch.stream.next()).await;
        assert!(extra.is_err(), "unexpected extra signal {extra:?}");
    }

    #[tokio::test(start_paused = true)]
    async fn a_heartbeat_repeats_the_latest_seq_every_30_seconds() {
        let notifier = Notifier::new();
        notifier.events_recorded(2);
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
        notifier.events_recorded(5);

        assert_eq!(watch.signal().await, 5);
        assert_eq!(watch.elapsed(), Duration::from_secs(20));
        assert_eq!(watch.signal().await, 5);
        assert_eq!(watch.elapsed(), Duration::from_secs(50));
    }

    #[tokio::test(start_paused = true)]
    async fn a_full_channel_collapses_into_the_newest_seq() {
        let notifier = Notifier::new();
        let mut watch = Watch::open(&notifier);
        assert_eq!(watch.signal().await, 0);

        notifier.events_recorded(8);
        settle().await;
        notifier.events_recorded(9);
        settle().await;
        notifier.events_recorded(10);
        settle().await;
        notifier.events_recorded(11);

        assert_eq!(watch.signal().await, 8);
        assert_eq!(watch.signal().await, 11);
        assert_eq!(watch.elapsed(), Duration::ZERO);
        let extra = timeout(HEARTBEAT - Duration::from_millis(1), watch.stream.next()).await;
        assert!(extra.is_err(), "unexpected extra signal {extra:?}");
    }

    #[tokio::test(start_paused = true)]
    async fn events_recorded_signals_even_when_the_seq_does_not_rise() {
        let notifier = Notifier::new();
        notifier.events_recorded(4);
        let mut watch = Watch::open(&notifier);
        assert_eq!(watch.signal().await, 4);

        notifier.events_recorded(4);
        assert_eq!(watch.signal().await, 4);
        assert_eq!(watch.elapsed(), Duration::ZERO);

        notifier.events_recorded(3);
        assert_eq!(watch.signal().await, 4);
        assert_eq!(watch.elapsed(), Duration::ZERO);
    }

    #[tokio::test(start_paused = true)]
    async fn status_changed_signals_with_the_same_seq() {
        let notifier = Notifier::new();
        notifier.events_recorded(4);
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
        assert_eq!(watch.elapsed(), Duration::ZERO);
    }

    #[tokio::test(start_paused = true)]
    async fn the_task_stops_when_the_hub_drops_the_stream() {
        let notifier = Notifier::new();
        let mut watch = Watch::open(&notifier);
        assert_eq!(watch.signal().await, 0);
        assert_eq!(notifier.sender.receiver_count(), 1);

        let Watch {
            stream,
            closing: _closing,
            ..
        } = watch;
        drop(stream);

        for _ in 0..100 {
            if notifier.sender.receiver_count() == 0 {
                return;
            }
            task::yield_now().await;
        }
        panic!("the watch task still holds a subscription after 100 yields");
    }

    #[tokio::test(start_paused = true)]
    async fn the_notifier_works_with_no_one_watching() {
        let notifier = Notifier::new();
        notifier.events_recorded(3);
        notifier.status_changed();

        let mut signals = notifier.subscribe();
        assert_eq!(signals.latest_seq(), 3);

        notifier.events_recorded(6);
        let changed = timeout(WAIT_LIMIT, signals.changed())
            .await
            .expect("changed resolves after events_recorded");
        assert_eq!(changed, 6);
    }

    #[tokio::test(start_paused = true)]
    async fn a_status_scope_signals_when_it_starts_and_when_it_ends() {
        let notifier = Notifier::new();
        let mut signals = notifier.subscribe();
        signals.latest_seq();

        let scope = notifier.status_scope();
        timeout(WAIT_LIMIT, signals.changed())
            .await
            .expect("the scope signals when it starts");
        drop(scope);
        timeout(WAIT_LIMIT, signals.changed())
            .await
            .expect("the scope signals when it ends");

        let extra = timeout(Duration::from_secs(1), signals.changed()).await;
        assert!(extra.is_err(), "unexpected extra signal {extra:?}");
    }

    async fn failing_cycle(notifier: &Notifier, signals: &mut Signals) -> Result<(), &'static str> {
        let _cycle = notifier.status_scope();
        timeout(WAIT_LIMIT, signals.changed())
            .await
            .expect("the scope signals when it starts");
        Err("the cycle failed")?;
        Ok(())
    }

    #[tokio::test(start_paused = true)]
    async fn a_status_scope_signals_again_when_the_cycle_returns_early() {
        let notifier = Notifier::new();
        let mut signals = notifier.subscribe();
        signals.latest_seq();

        let cycle = failing_cycle(&notifier, &mut signals).await;

        assert_eq!(cycle, Err("the cycle failed"));
        timeout(WAIT_LIMIT, signals.changed())
            .await
            .expect("the scope signals again when the cycle returns early");
    }

    #[tokio::test(start_paused = true)]
    async fn a_status_scope_signals_again_when_the_cycle_panics() {
        let notifier = Notifier::new();
        let mut signals = notifier.subscribe();
        signals.latest_seq();

        let cycle = panic::catch_unwind(AssertUnwindSafe(|| {
            let _cycle = notifier.status_scope();
            signals.latest_seq();
            panic!("the cycle panicked");
        }));

        assert!(cycle.is_err(), "the cycle did not panic");
        timeout(WAIT_LIMIT, signals.changed())
            .await
            .expect("the scope signals again when the cycle panics");
    }
}
