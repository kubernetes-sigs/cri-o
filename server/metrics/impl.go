package metrics

import (
	"crypto/tls"
	"net"
	"net/http"

	"github.com/cri-o/cri-o/pkg/config"
	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus"
)

type defaultImpl struct{}

type Impl interface {
	Register(prometheus.Collector) error
	RemoveUnusedSocket(string) error
	Listen(string, string) (net.Listener, error)
	ServeTLS(*http.Server, net.Listener, string, string) error
	Serve(net.Listener, http.Handler) error
	LoadX509KeyPair(string, string) (tls.Certificate, error)
	NewWatcher() (*fsnotify.Watcher, error)
	Add(*fsnotify.Watcher, string) error
}

func (d *defaultImpl) Register(c prometheus.Collector) error {
	return prometheus.Register(c)
}

func (d *defaultImpl) RemoveUnusedSocket(path string) error {
	return config.RemoveUnusedSocket(path)
}

func (d *defaultImpl) Listen(network, address string) (net.Listener, error) {
	return net.Listen(network, address)
}

func (d *defaultImpl) ServeTLS(srv *http.Server, l net.Listener, certFile, keyFile string) error {
	return srv.ServeTLS(l, certFile, keyFile)
}

func (d *defaultImpl) Serve(l net.Listener, handler http.Handler) error {
	return http.Serve(l, handler)
}

func (d *defaultImpl) LoadX509KeyPair(certFile, keyFile string) (tls.Certificate, error) {
	return tls.LoadX509KeyPair(certFile, keyFile)
}

func (d *defaultImpl) NewWatcher() (*fsnotify.Watcher, error) {
	return fsnotify.NewWatcher()
}

func (d *defaultImpl) Add(watcher *fsnotify.Watcher, name string) error {
	return watcher.Add(name)
}
