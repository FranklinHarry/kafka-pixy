package topiccsm

import (
	"fmt"
	"sync"
	"time"

	"github.com/mailgun/kafka-pixy/actor"
	"github.com/mailgun/kafka-pixy/config"
	"github.com/mailgun/kafka-pixy/consumer"
	"github.com/mailgun/kafka-pixy/consumer/dispatcher"
)

var requestTimeoutRs = dispatcher.Response{Err: consumer.ErrRequestTimeout}

// T implements a consumer request dispatch tier responsible for a particular
// topic. It receives requests on the `Requests()` channel and replies with
// messages received on `Messages()` channel. If there has been no message
// received for `Config.Consumer.LongPollingTimeout` then a timeout error is
// sent to the requests' reply channel.
//
// implements `dispatcher.Tier`.
// implements `multiplexer.Out`.
type T struct {
	actDesc            *actor.Descriptor
	childSpec          dispatcher.ChildSpec
	cfg                *config.Proxy
	group              string
	topic              string
	expireTimer        *time.Timer
	nilOrExpireTimerCh <-chan time.Time
	lifespanCh         chan<- *T
	messagesCh         chan consumer.Message
	wg                 sync.WaitGroup
}

// Creates a topic consumer instance. It should be explicitly started in
// accordance with the `dispatcher.Tier` contract.
func Spawn(parentActDesc *actor.Descriptor, group string, childSpec dispatcher.ChildSpec, cfg *config.Proxy, lifespanCh chan<- *T) *T {
	topic := string(childSpec.Key())
	actDesc := parentActDesc.NewChild(fmt.Sprintf("%s", topic))
	actDesc.AddLogField("kafka.group", group)
	actDesc.AddLogField("kafka.topic", topic)
	tc := T{
		actDesc:    actDesc,
		childSpec:  childSpec,
		cfg:        cfg,
		group:      group,
		topic:      topic,
		lifespanCh: lifespanCh,

		// Messages channel must be non-buffered. Otherwise we might end up
		// buffering a message from a partition that no longer belongs to this
		// consumer group member.
		messagesCh: make(chan consumer.Message),
	}
	actor.Spawn(tc.actDesc, &tc.wg, tc.run)
	return &tc
}

// Topic returns the topic name this topic consumer is responsible for.
func (tc *T) Topic() string {
	return tc.topic
}

// implements `multiplexer.Out`
func (tc *T) Messages() chan<- consumer.Message {
	return tc.messagesCh
}

func (tc *T) run() {
	defer tc.childSpec.Dispose()
	tc.lifespanCh <- tc
	defer func() {
		tc.lifespanCh <- tc
	}()

	for {
		select {
		case consumeReq, ok := <-tc.childSpec.Requests():
			if !ok {
				tc.actDesc.Log().Info("Signaled to shutdown")
				return
			}
			tc.stopExpireTimer()

			requestAge := time.Now().UTC().Sub(consumeReq.Timestamp)
			ttl := tc.cfg.Consumer.LongPollingTimeout - requestAge
			// The request has been waiting in the buffer for too long. If we
			// reply with a fetched message, then there is a good chance that the
			// client won't receive it due to the client HTTP timeout. Therefore
			// we reject the request to avoid message loss.
			if ttl <= 0 {
				consumeReq.ResponseCh <- requestTimeoutRs
				continue
			}
			select {
			case msg := <-tc.messagesCh:
				msg.EventsCh <- consumer.Event{consumer.EvOffered, msg.Offset}
				consumeReq.ResponseCh <- dispatcher.Response{Msg: msg}
			case <-time.After(ttl):
				consumeReq.ResponseCh <- requestTimeoutRs
			}
		case <-tc.nilOrExpireTimerCh:
			tc.actDesc.Log().Info("Topic registration expired")
			return
		default:
			tc.ensureExpireTimer()
		}
	}
}

func (tc *T) String() string {
	return tc.actDesc.String()
}

func (tc *T) stopExpireTimer() {
	if tc.expireTimer == nil {
		return
	}
	if !tc.expireTimer.Stop() {
		<-tc.expireTimer.C
	}
	tc.nilOrExpireTimerCh = nil
}

func (tc *T) ensureExpireTimer() {
	if tc.nilOrExpireTimerCh != nil {
		return
	}
	if tc.expireTimer == nil {
		tc.expireTimer = time.NewTimer(tc.cfg.Consumer.SubscriptionTimeout)
	} else {
		tc.expireTimer.Reset(tc.cfg.Consumer.SubscriptionTimeout)
	}
	tc.nilOrExpireTimerCh = tc.expireTimer.C
}
