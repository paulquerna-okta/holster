package etcdutil

import (
	"context"
	"sync"
	"time"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/mailgun/holster"
	"github.com/pkg/errors"
)

const NoLease = etcd.LeaseID(-1)

type SessionObserver func(etcd.LeaseID, error)

type Session struct {
	keepAlive     <-chan *etcd.LeaseKeepAliveResponse
	lease         *etcd.LeaseGrantResponse
	backOff       *holster.BackOffCounter
	wg            holster.WaitGroup
	ctx           context.Context
	cancel        context.CancelFunc
	conf          SessionConfig
	client        *etcd.Client
	timeout       time.Duration
	once          *sync.Once
	lastKeepAlive time.Time
}

type SessionConfig struct {
	TTL      int64
	Observer SessionObserver
}

// NewSession creates a lease and monitors lease keep alive's for connectivity.
// Once a lease ID is granted SessionConfig.Observer is called with the granted lease.
// If connectivity is lost with etcd SessionConfig.Observer is called again with -1 (NoLease)
// as the lease ID. The Session will continue to try to gain another lease, once a new lease
// is gained SessionConfig.Observer is called again with the new lease id.
func NewSession(c *etcd.Client, conf SessionConfig) (*Session, error) {
	holster.SetDefault(&conf.TTL, int64(30))

	if conf.Observer == nil {
		return nil, errors.New("provided observer function cannot be nil")
	}

	if c == nil {
		return nil, errors.New("provided etcd client cannot be nil")
	}

	s := Session{
		timeout: time.Second * time.Duration(conf.TTL),
		backOff: holster.NewBackOff(time.Millisecond*500, time.Duration(conf.TTL)*time.Second, 2),
		once:    &sync.Once{},
		conf:    conf,
		client:  c,
	}

	s.run()
	return &s, nil
}

func (s *Session) run() {
	s.ctx, s.cancel = context.WithCancel(context.Background())
	ticker := time.NewTicker(s.timeout)
	s.lastKeepAlive = time.Now()

	s.wg.Until(func(done chan struct{}) bool {
		// If we have lost our keep alive, attempt to regain it
		if s.keepAlive == nil {
			if err := s.gainLease(s.ctx); err != nil {
				s.conf.Observer(NoLease, errors.Wrap(err, "while attempting to gain new lease"))
				select {
				case <-time.After(s.backOff.Next()):
					return true
				case <-s.ctx.Done():
					return false
				}
				return true
			}
		}
		s.backOff.Reset()

		select {
		case _, ok := <-s.keepAlive:
			if !ok {
				//logrus.Warn("heartbeat lost")
				s.keepAlive = nil
			} else {
				//logrus.Debug("heartbeat received")
				s.lastKeepAlive = time.Now()
			}
		case <-ticker.C:
			// Ensure we are getting heartbeats regularly
			if time.Now().Sub(s.lastKeepAlive) > s.timeout {
				//logrus.Warn("too long between heartbeats")
				s.keepAlive = nil
			}
		case <-done:
			if s.lease != nil {
				ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
				if _, err := s.client.Revoke(ctx, s.lease.ID); err != nil {
					s.conf.Observer(NoLease, errors.Wrap(err, "while revoking our lease during shutdown"))
				}
				cancel()
			}
			return false
		}

		if s.keepAlive == nil {
			s.conf.Observer(NoLease, nil)
		}
		return true
	})
}

func (s *Session) Reset(ctx context.Context) {
	s.Close()
	s.once = &sync.Once{}
	s.run()
}

// Close terminates the session shutting down all network operations,
// then SessionConfig.Observer is called with -1 (NoLease), only returns
// once the session has closed successfully.
func (s *Session) Close() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Stop()
		s.conf.Observer(NoLease, nil)
	})
}

func (s *Session) gainLease(ctx context.Context) error {
	//logrus.Debug("attempting to grant new lease")
	lease, err := s.client.Grant(ctx, s.conf.TTL)
	if err != nil {
		return errors.Wrapf(err, "during grant lease")
	}

	s.keepAlive, err = s.client.KeepAlive(s.ctx, lease.ID)
	if err != nil {
		return err
	}
	//logrus.Debugf("new lease %d", lease.ID)
	s.conf.Observer(lease.ID, nil)
	return nil
}