package consumers

import "time"

type Consumer struct {
	Name      string     `json:"name"`
	Position  int64      `json:"position" doc:"The seq its next stream starts after: the newest event when it was created, then the highest seq it acknowledged."`
	Behind    int64      `json:"behind" doc:"How many events after the position it has not acknowledged, baseline events left out."`
	AckedAt   *time.Time `json:"acked_at" doc:"When it last acknowledged; null if it never did."`
	SeenAt    *time.Time `json:"seen_at" doc:"When the hub last heard from it, on an acknowledgement or a stream ping; null if never."`
	Connected bool       `json:"connected" doc:"Whether it holds a stream open now."`
}

type ListOutput struct {
	Body struct {
		Items []Consumer `json:"items"`
	}
}
