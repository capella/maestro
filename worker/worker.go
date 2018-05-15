// maestro
// https://github.com/topfreegames/maestro
//
// Licensed under the MIT license:
// http://www.opensource.org/licenses/mit-license
// Copyright © 2017 Top Free Games <backend@tfgco.com>

package worker

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/spf13/viper"
	redis "github.com/topfreegames/extensions/redis"
	"github.com/topfreegames/maestro/controller"
	"github.com/topfreegames/maestro/eventforwarder"
	"github.com/topfreegames/maestro/extensions"
	"github.com/topfreegames/maestro/metadata"
	"github.com/topfreegames/maestro/models"
	"github.com/topfreegames/maestro/watcher"
	"k8s.io/client-go/kubernetes"

	pginterfaces "github.com/topfreegames/extensions/pg/interfaces"
	redisinterfaces "github.com/topfreegames/extensions/redis/interfaces"
)

type gracefulShutdown struct {
	wg      *sync.WaitGroup
	timeout time.Duration
}

// Worker struct for worker
type Worker struct {
	Config           *viper.Viper
	DB               pginterfaces.DB
	InCluster        bool
	KubeconfigPath   string
	KubernetesClient kubernetes.Interface
	Logger           logrus.FieldLogger
	MetricsReporter  *models.MixedMetricsReporter
	RedisClient      *redis.Client
	Run              bool
	SyncPeriod       int
	Watchers         map[string]*watcher.Watcher
	gracefulShutdown *gracefulShutdown
	Forwarders       []*eventforwarder.Info
	getLocksTimeout  int
	lockTimeoutMs    int
}

// NewWorker is the worker constructor
func NewWorker(
	config *viper.Viper,
	logger logrus.FieldLogger,
	mr *models.MixedMetricsReporter,
	incluster bool,
	kubeconfigPath string,
	dbOrNil pginterfaces.DB,
	redisClientOrNil redisinterfaces.RedisClient,
	kubernetesClientOrNil kubernetes.Interface,
) (*Worker, error) {
	w := &Worker{
		Config:          config,
		Logger:          logger,
		MetricsReporter: mr,
		InCluster:       incluster,
		KubeconfigPath:  kubeconfigPath,
	}

	err := w.configure(dbOrNil, redisClientOrNil, kubernetesClientOrNil)
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Worker) loadConfigurationDefaults() {
	w.Config.SetDefault("worker.syncPeriod", 10)
	w.Config.SetDefault("worker.gracefulShutdownTimeout", 300)
	w.Config.SetDefault("worker.retrieveFreePortsPeriod", 3600)
	w.Config.SetDefault("worker.getLocksTimeout", 300)
	w.Config.SetDefault("worker.lockTimeoutMs", 180000)
}

func (w *Worker) configure(dbOrNil pginterfaces.DB, redisClientOrNil redisinterfaces.RedisClient, kubernetesClientOrNil kubernetes.Interface) error {
	w.loadConfigurationDefaults()
	w.configureLogger()
	w.configureForwarders()

	w.SyncPeriod = w.Config.GetInt("worker.syncPeriod")
	w.getLocksTimeout = w.Config.GetInt("worker.getLocksTimeout")
	w.lockTimeoutMs = w.Config.GetInt("worker.lockTimeoutMs")
	w.Watchers = make(map[string]*watcher.Watcher)
	var wg sync.WaitGroup
	w.gracefulShutdown = &gracefulShutdown{
		wg:      &wg,
		timeout: time.Duration(w.Config.GetInt("worker.gracefulShutdownTimeout")) * time.Second,
	}

	err := w.configureDatabase(dbOrNil)
	if err != nil {
		return err
	}

	err = w.configureRedisClient(redisClientOrNil)
	if err != nil {
		return err
	}

	err = w.configureKubernetesClient(kubernetesClientOrNil)
	if err != nil {
		return err
	}

	return nil
}

func (w *Worker) configureForwarders() {
	w.Forwarders = eventforwarder.LoadEventForwardersFromConfig(w.Config, w.Logger)
}

func (w *Worker) configureKubernetesClient(kubernetesClientOrNil kubernetes.Interface) error {
	if kubernetesClientOrNil != nil {
		w.KubernetesClient = kubernetesClientOrNil
		return nil
	}
	clientset, err := extensions.GetKubernetesClient(w.Logger, w.InCluster, w.KubeconfigPath)
	if err != nil {
		return err
	}
	w.KubernetesClient = clientset
	return nil
}

func (w *Worker) configureDatabase(dbOrNil pginterfaces.DB) error {
	if dbOrNil != nil {
		w.DB = dbOrNil
		return nil
	}
	db, err := extensions.GetDB(w.Logger, w.Config)
	if err != nil {
		return err
	}

	w.DB = db
	return nil
}

func (w *Worker) configureRedisClient(redisClientOrNil redisinterfaces.RedisClient) error {
	if redisClientOrNil != nil {
		redisClient, err := redis.NewClient("extensions.redis", w.Config, redisClientOrNil)
		if err != nil {
			return err
		}
		w.RedisClient = redisClient
		return nil
	}
	redisClient, err := extensions.GetRedisClient(w.Logger, w.Config)
	if err != nil {
		return err
	}
	w.RedisClient = redisClient
	return nil
}

func (w *Worker) configureLogger() {
	w.Logger = w.Logger.WithFields(logrus.Fields{
		"source":  "maestro-worker",
		"version": metadata.Version,
	})
}

func (w *Worker) savePortRangeOnRedis(start, end int) error {
	portsRange := fmt.Sprintf("%d-%d", start, end)
	return w.RedisClient.Client.Set(models.GlobalPortsPoolKey, portsRange, 0).Err()
}

// Start starts the worker
func (w *Worker) Start(startHostPortRange, endHostPortRange int, showProfile bool) error {
	l := w.Logger.WithFields(logrus.Fields{
		"operation": "start",
	})

	if showProfile {
		go func() {
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
	}

	w.Run = true
	sigchan := make(chan os.Signal)
	signal.Notify(sigchan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(time.Duration(w.SyncPeriod) * time.Second)
	defer ticker.Stop()

	err := w.savePortRangeOnRedis(startHostPortRange, endHostPortRange)
	if err != nil {
		return err
	}

	for w.Run == true {
		select {
		case <-ticker.C:
			err = w.savePortRangeOnRedis(startHostPortRange, endHostPortRange)
			if err != nil {
				l.WithError(err).Error("error saving ports on redis")
				return err
			}

			schedulerNames, err := controller.ListSchedulersNames(l, w.MetricsReporter, w.DB)
			if err != nil {
				l.WithError(err).Error("error listing schedulers")
				return err
			}
			w.EnsureRunningWatchers(schedulerNames)
			w.RemoveDeadWatchers()

			l.Infof("number of goroutines: %d", runtime.NumGoroutine())
			l.Infof("number of watchers: %d", len(w.Watchers))
		case sig := <-sigchan:
			l.Warnf("caught signal %v: terminating\n", sig)
			w.Run = false
		}
	}

	extensions.GracefulShutdown(l, w.gracefulShutdown.wg, w.gracefulShutdown.timeout)
	return nil
}

// EnsureRunningWatchers ensures all schedulers have running watchers
func (w *Worker) EnsureRunningWatchers(schedulerNames []string) {
	w.gracefulShutdown.wg.Add(1)
	defer w.gracefulShutdown.wg.Done()

	l := w.Logger.WithFields(logrus.Fields{
		"operation": "ensureRunningWatchers",
	})
	for _, schedulerName := range schedulerNames {
		if schedulerWatcher, ok := w.Watchers[schedulerName]; ok {
			// ensure schedulers in the database have running watchers
			if !schedulerWatcher.Run {
				schedulerWatcher.Run = true
				go schedulerWatcher.Start()
			}
		} else {
			var occupiedTimeout int64
			var configYaml *models.ConfigYAML
			var gameName string

			configYamlStr, err := models.LoadConfig(w.DB, schedulerName)
			if err == nil {
				configYaml, err = models.NewConfigYAML(configYamlStr)
				if err == nil {
					occupiedTimeout = configYaml.OccupiedTimeout
					gameName = configYaml.Game
				}
			}
			if err != nil {
				l.Warnf("error loading scheduler %s: %s", schedulerName, err.Error())
				occupiedTimeout = w.Config.GetInt64("occupiedTimeout")
				gameName = w.Config.GetString("game")
			}
			// create and start a watcher if necessary
			w.Watchers[schedulerName] = watcher.NewWatcher(
				w.Config,
				w.Logger,
				w.MetricsReporter,
				w.DB,
				w.RedisClient,
				w.KubernetesClient,
				schedulerName,
				gameName,
				occupiedTimeout,
				w.Forwarders,
			)
			w.Watchers[schedulerName].Run = true // Avoids race condition
			go w.Watchers[schedulerName].Start()
			l.WithField("name", schedulerName).Info("started watcher for scheduler")
		}
	}
}

// RemoveDeadWatchers removes dead watchers from worker watcher map
func (w *Worker) RemoveDeadWatchers() {
	w.gracefulShutdown.wg.Add(1)
	defer w.gracefulShutdown.wg.Done()

	l := w.Logger.WithFields(logrus.Fields{
		"operation": "removeDeadWatchers",
	})
	// remove dead watchers from worker watcher map
	for schedulerName, schedulerWatcher := range w.Watchers {
		if !schedulerWatcher.Run {
			l.WithField("name", schedulerName).Info("removed watcher for scheduler")
			delete(w.Watchers, schedulerName)
		}
	}
}
