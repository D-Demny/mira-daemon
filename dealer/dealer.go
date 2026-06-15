package dealer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/coder/websocket"
	librespot "github.com/devgianlu/go-librespot"
)

const (
	pingInterval = 30 * time.Second
	timeout      = 10 * time.Second
)

var ErrDealerClosed = errors.New("dealer closed")

type Dealer struct {
	log librespot.Logger

	client *http.Client

	addr        librespot.GetAddressFunc
	accessToken librespot.GetLogin5TokenFunc

	conn *websocket.Conn

	done         chan struct{}
	closeOnce    sync.Once
	recvLoopOnce sync.Once
	lastPong     time.Time
	lastPongLock sync.Mutex

	// reconnectNow wakes the reconnect loop out of a backoff sleep
	reconnectNow chan struct{}

	// connMu protects conn pointer state.
	connMu sync.RWMutex

	messageReceivers     []messageReceiver
	messageReceiversLock sync.RWMutex

	requestReceivers     map[string]requestReceiver
	requestReceiversLock sync.RWMutex
}

func NewDealer(log librespot.Logger, client *http.Client, dealerAddr librespot.GetAddressFunc, accessToken librespot.GetLogin5TokenFunc) *Dealer {
	return &Dealer{
		client: &http.Client{
			Transport:     client.Transport,
			CheckRedirect: client.CheckRedirect,
			Jar:           client.Jar,
			Timeout:       timeout,
		},
		log:              log,
		addr:             dealerAddr,
		accessToken:      accessToken,
		done:             make(chan struct{}),
		reconnectNow:     make(chan struct{}, 1),
		requestReceivers: map[string]requestReceiver{},
	}
}

func (d *Dealer) Connect(ctx context.Context) error {
	d.connMu.Lock()
	defer d.connMu.Unlock()

	select {
	case <-d.done:
		return ErrDealerClosed
	default:
	}

	if d.conn != nil {
		d.log.Debugf("dealer connection already opened")
		return nil
	}

	return d.connect(ctx)
}

func (d *Dealer) connect(ctx context.Context) error {
	accessToken, err := d.accessToken(ctx, false)
	if err != nil {
		return fmt.Errorf("failed obtaining dealer access token: %w", err)
	}

	addr := d.addr(ctx)
	if conn, _, err := websocket.Dial(ctx, fmt.Sprintf("wss://%s/?access_token=%s", addr, accessToken), &websocket.DialOptions{
		HTTPClient: d.client,
		HTTPHeader: http.Header{
			"User-Agent": []string{librespot.UserAgent()},
		},
	}); err != nil {
		return err
	} else {
		if d.conn != nil {
			// CloseNow, not graceful Close
			_ = d.conn.CloseNow()
		}

		// we assign to d.conn after because if Dial fails we'll have a nil d.conn which we don't want
		d.conn = conn
		d.log.Debug(fmt.Sprintf("connected to %s", addr))
	}

	// remove the read limit
	d.conn.SetReadLimit(math.MaxUint32)

	return nil
}

func (d *Dealer) Close() {
	d.closeOnce.Do(func() {
		close(d.done)
		d.closeConn(websocket.StatusGoingAway)
	})
}

// ForceReconnect closes the current connection and kicks the reconnect loop
func (d *Dealer) ForceReconnect() {
	select {
	case <-d.done:
		return
	default:
	}
	d.log.Debugf("forcing dealer reconnect")

	// wake the reconnect loop out of its backoff sleep 
	select {
	case d.reconnectNow <- struct{}{}:
	default:
	}


	if d.connMu.TryRLock() {
		conn := d.conn
		d.connMu.RUnlock()
		if conn != nil {
			// CloseNow tears down the underlying TCP immediately
			_ = conn.CloseNow()
		}
	}
}

func (d *Dealer) startReceiving() {
	d.recvLoopOnce.Do(func() {
		d.log.Tracef("starting dealer recv loop")
		d.resetPongDeadline()
		go d.pingTicker()
		go d.recvLoop()
	})
}

func (d *Dealer) pingTicker() {
	ticker := time.NewTicker(pingInterval)

loop:
	for {
		select {
		case <-d.done:
			break loop
		case <-ticker.C:
			timePassed := d.timeSinceLastPong()
			if timePassed > pingInterval+timeout {
				d.log.Errorf("did not receive last pong from dealer, %.0fs passed", timePassed.Seconds())

				// closing the connection should make the read on the "recvLoop" fail,
				// continue hoping for a new connection
				d.closeConn(websocket.StatusServiceRestart)
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			conn, err := d.writeConn(ctx, websocket.MessageText, []byte("{\"type\":\"ping\"}"))
			cancel()
			d.log.Tracef("sent dealer ping")

			if err != nil {
				select {
				case <-d.done:
					break loop
				default:
				}

				d.log.WithError(err).Warnf("failed sending dealer ping")

				// closing the connection should make the read on the "recvLoop" fail,
				// continue hoping for a new connection
				d.closeConnRef(conn, websocket.StatusServiceRestart)
				continue
			}
		}
	}

	ticker.Stop()
}

func (d *Dealer) recvLoop() {
loop:
	for {
		select {
		case <-d.done:
			break loop
		default:
			// no need to hold the connMu since reconnection happens in this routine
			msgType, messageBytes, err := d.readConn(context.Background())

			// don't log closed error if we're shutting down
			if err != nil {
				select {
				case <-d.done:
					if websocket.CloseStatus(err) == websocket.StatusGoingAway {
						d.log.Debugf("dealer connection closed")
					}
					break loop
				default:
				}

				d.log.WithError(err).Errorf("failed receiving dealer message")
				break loop
			} else if msgType != websocket.MessageText {
				d.log.WithError(err).Warnf("unsupported message type: %v, len: %d", msgType, len(messageBytes))
				continue
			}

			var message RawMessage
			if err := json.Unmarshal(messageBytes, &message); err != nil {
				d.log.WithError(err).Error("failed unmarshalling dealer message")
				break loop
			}

			switch message.Type {
			case "message":
				d.handleMessage(&message)
				break
			case "request":
				d.handleRequest(&message)
				break
			case "ping":
				// we never receive ping messages
				break
			case "pong":
				d.lastPongLock.Lock()
				d.lastPong = time.Now()
				d.lastPongLock.Unlock()
				d.log.Tracef("received dealer pong")
				break
			default:
				d.log.Warnf("unknown dealer message type: %s", message.Type)
				break
			}
		}
	}

	// always close as we might end up here because of application errors
	d.closeConn(websocket.StatusInternalError)

	select {
	case <-d.done:
	default:
		// must keep retrying across long network outages
		bo := backoff.NewExponentialBackOff()
		bo.MaxElapsedTime = 0
	retryLoop:
		for {
			d.connMu.Lock()
			err := d.reconnect()
			d.connMu.Unlock()
			if err == nil {
				// reconnection was successful, do not close receivers
				return
			} else if errors.Is(err, ErrDealerClosed) {
				break
			}

			select {
			case <-d.done:
				break retryLoop
			case <-d.reconnectNow:
				// resume recovery says the network just came back, retry immediately
				bo.Reset()
			case <-time.After(bo.NextBackOff()):
			}
		}
	}

	d.requestReceiversLock.RLock()
	for _, recv := range d.requestReceivers {
		close(recv.c)
	}
	d.requestReceiversLock.RUnlock()

	d.messageReceiversLock.RLock()
	for _, recv := range d.messageReceivers {
		close(recv.c)
	}
	d.messageReceiversLock.RUnlock()

	d.log.Debugf("dealer recv loop stopped")
}

func (d *Dealer) sendReply(key string, success bool) error {
	reply := Reply{Type: "reply", Key: key}
	reply.Payload.Success = success

	replyBytes, err := json.Marshal(reply)
	if err != nil {
		return fmt.Errorf("failed marshalling reply: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	_, err = d.writeConn(ctx, websocket.MessageText, replyBytes)
	cancel()
	if err != nil {
		return fmt.Errorf("failed sending dealer reply: %w", err)
	}

	return nil
}

func (d *Dealer) reconnect() error {
	// stop the (now infinite) retry loop if we're shutting down
	select {
	case <-d.done:
		return backoff.Permanent(ErrDealerClosed)
	default:
	}
	if err := d.connect(context.TODO()); err != nil {
		return err
	}

	d.resetPongDeadline()
	// restart the recv loop
	go d.recvLoop()

	d.log.Info("re-established dealer connection")
	return nil
}

func (d *Dealer) resetPongDeadline() {
	d.lastPongLock.Lock()
	d.lastPong = time.Now().Add(pingInterval)
	d.lastPongLock.Unlock()
}

func (d *Dealer) timeSinceLastPong() time.Duration {
	d.lastPongLock.Lock()
	defer d.lastPongLock.Unlock()
	return time.Since(d.lastPong)
}

func (d *Dealer) closeConn(status websocket.StatusCode) {
	d.connMu.RLock()
	conn := d.conn
	d.connMu.RUnlock()

	d.closeConnRef(conn, status)
}

func (d *Dealer) closeConnRef(conn *websocket.Conn, status websocket.StatusCode) {
	// CloseNow (forceful TCP teardown)
	if conn != nil {
		_ = conn.CloseNow()
	}
}

func (d *Dealer) writeConn(ctx context.Context, typ websocket.MessageType, payload []byte) (*websocket.Conn, error) {
	d.connMu.RLock()
	select {
	case <-d.done:
		d.connMu.RUnlock()
		return nil, ErrDealerClosed
	default:
	}

	conn := d.conn
	d.connMu.RUnlock()

	if conn == nil {
		return nil, fmt.Errorf("dealer connection not established")
	}

	err := conn.Write(ctx, typ, payload)
	if err != nil {
		select {
		case <-d.done:
			return conn, ErrDealerClosed
		default:
		}
	}

	return conn, err
}

func (d *Dealer) readConn(ctx context.Context) (websocket.MessageType, []byte, error) {
	d.connMu.RLock()
	conn := d.conn
	d.connMu.RUnlock()

	if conn == nil {
		return 0, nil, fmt.Errorf("dealer connection not established")
	}

	return conn.Read(ctx)
}
