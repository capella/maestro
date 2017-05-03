package controller_test

import (
	"fmt"
	"strconv"
	"time"

	"github.com/go-redis/redis"
	"github.com/golang/mock/gomock"
	"github.com/topfreegames/maestro/controller"
	"github.com/topfreegames/maestro/models"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	yaml "gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	yaml1 = `
name: controller-name
game: controller
image: controller/controller:v123
ports:
  - containerPort: 1234
    protocol: UDP
    name: port1
  - containerPort: 7654
    protocol: TCP
    name: port2
limits:
  memory: "66Mi"
  cpu: "2"
shutdownTimeout: 20
autoscaling:
  min: 3
  up:
    delta: 2
    trigger:
      usage: 60
      time: 100
    cooldown: 200
  down:
    delta: 1
    trigger:
      usage: 30
      time: 500
    cooldown: 500
env:
  - name: MY_ENV_VAR
    value: myvalue
cmd:
  - "./room"
`
)

var _ = Describe("Controller", func() {
	var (
		clientset *fake.Clientset
	)

	BeforeEach(func() {
		clientset = fake.NewSimpleClientset()
	})

	Describe("CreateScheduler", func() {
		It("should succeed", func() {
			var configYaml1 models.ConfigYAML
			err := yaml.Unmarshal([]byte(yaml1), &configYaml1)
			Expect(err).NotTo(HaveOccurred())
			mockRedisClient.EXPECT().TxPipeline().Return(mockPipeline).Times(configYaml1.AutoScaling.Min)
			mockPipeline.EXPECT().HMSet(gomock.Any(), map[string]interface{}{
				"status":   "creating",
				"lastPing": int64(0),
			}).Times(configYaml1.AutoScaling.Min)
			mockPipeline.EXPECT().SAdd(models.GetRoomStatusSetRedisKey(configYaml1.Name, "creating"), gomock.Any()).Times(configYaml1.AutoScaling.Min)
			mockPipeline.EXPECT().Exec().Times(configYaml1.AutoScaling.Min)
			db.EXPECT().Query(gomock.Any(), "INSERT INTO schedulers (name, game, yaml) VALUES (?name, ?game, ?yaml) RETURNING id", gomock.Any())
			db.EXPECT().Query(gomock.Any(), "SELECT * FROM schedulers WHERE name = ?", configYaml1.Name).Do(func(scheduler *models.Scheduler, query string, modifier string) {
				scheduler.YAML = yaml1
			})
			err = controller.CreateScheduler(logger, mr, db, mockRedisClient, clientset, &configYaml1)
			Expect(err).NotTo(HaveOccurred())

			ns, err := clientset.CoreV1().Namespaces().List(metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(ns.Items).To(HaveLen(1))
			Expect(ns.Items[0].GetName()).To(Equal("controller-name"))

			svcs, err := clientset.CoreV1().Services("controller-name").List(metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(svcs.Items).To(HaveLen(3))

			for _, svc := range svcs.Items {
				Expect(svc.GetName()).To(ContainSubstring("controller-name-"))
				Expect(svc.GetName()).To(HaveLen(len("controller-name-") + 8))
			}

			pods, err := clientset.CoreV1().Pods("controller-name").List(metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(pods.Items).To(HaveLen(3))
			for _, pod := range pods.Items {
				Expect(pod.GetName()).To(ContainSubstring("controller-name-"))
				Expect(pod.GetName()).To(HaveLen(len("controller-name-") + 8))
				Expect(pod.Spec.Containers[0].Env[1].Name).To(Equal("MAESTRO_SCHEDULER_NAME"))
				Expect(pod.Spec.Containers[0].Env[1].Value).To(Equal("controller-name"))
				Expect(pod.Spec.Containers[0].Env[2].Name).To(Equal("MAESTRO_ROOM_ID"))
				Expect(pod.Spec.Containers[0].Env[2].Value).To(Equal(pod.GetName()))
				Expect(pod.Spec.Containers[0].Env[3].Name).To(Equal("MAESTRO_NODE_PORT_1234_UDP"))
				Expect(pod.Spec.Containers[0].Env[3].Value).NotTo(BeNil())
				Expect(pod.Spec.Containers[0].Env[4].Name).To(Equal("MAESTRO_NODE_PORT_7654_TCP"))
				Expect(pod.Spec.Containers[0].Env[4].Value).NotTo(BeNil())
			}
		})

		It("should rollback if error in db occurs", func() {
			var configYaml1 models.ConfigYAML
			err := yaml.Unmarshal([]byte(yaml1), &configYaml1)
			Expect(err).NotTo(HaveOccurred())
			err = controller.CreateScheduler(logger, mr, db, mockRedisClient, clientset, &configYaml1)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("Some error in db"))

			ns, err := clientset.CoreV1().Namespaces().List(metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(ns.Items).To(HaveLen(0))

			svcs, err := clientset.CoreV1().Services("controller-name").List(metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(svcs.Items).To(HaveLen(0))

			pods, err := clientset.CoreV1().Pods("controller-name").List(metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(pods.Items).To(HaveLen(0))
		})

		It("should rollback if error in kubernetes occurs", func() {
			// TODO: test it later
		})

	})

	Describe("DeleteScheduler", func() {
		It("should succeed", func() {
			var configYaml1 models.ConfigYAML
			err := yaml.Unmarshal([]byte(yaml1), &configYaml1)
			Expect(err).NotTo(HaveOccurred())
			mockRedisClient.EXPECT().TxPipeline()
			err = controller.CreateScheduler(logger, mr, db, mockRedisClient, clientset, &configYaml1)
			Expect(err).NotTo(HaveOccurred())

			err = controller.DeleteScheduler(logger, mr, db, clientset, "controller-name")
			Expect(err).NotTo(HaveOccurred())
			ns, err := clientset.CoreV1().Namespaces().List(metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(ns.Items).To(HaveLen(0))
		})
	})

	Describe("GetSchedulerScalingInfo", func() {
		It("should succeed", func() {
			var configYaml1 models.ConfigYAML
			err := yaml.Unmarshal([]byte(yaml1), &configYaml1)
			Expect(err).NotTo(HaveOccurred())
			mockRedisClient.EXPECT().TxPipeline()
			err = controller.CreateScheduler(logger, mr, db, mockRedisClient, clientset, &configYaml1)
			Expect(err).NotTo(HaveOccurred())

			_, _, err = controller.GetSchedulerScalingInfo(logger, mr, db, mockRedisClient, "controller-name")
			Expect(err).NotTo(HaveOccurred())
			// TODO: test returned info
		})

		It("should fail if error in db", func() {
			_, _, err := controller.GetSchedulerScalingInfo(logger, mr, db, mockRedisClient, "controller-name")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("Some error in db"))
		})
	})

	Describe("SaveSchedulerStateInfo", func() {
		It("should succeed", func() {
			name := "pong-free-for-all"
			state := "in-sync"
			lastChangedAt := time.Now().Unix()
			lastScaleAt := time.Now().Unix()
			mockRedisClient.EXPECT().HMSet(name, map[string]interface{}{
				"state":         state,
				"lastChangedAt": lastChangedAt,
				"lastScaleOpAt": lastScaleAt,
			}).Return(&redis.StatusCmd{})
			schedulerState := models.NewSchedulerState(name, state, lastChangedAt, lastScaleAt)
			err = controller.SaveSchedulerStateInfo(logger, mr, mockRedisClient, schedulerState)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should fail if error in redis", func() {
			name := "controller-name"
			state := "in-sync"
			lastChangedAt := time.Now().Unix()
			lastScaleAt := time.Now().Unix()
			mockRedisClient.EXPECT().HMSet(name, map[string]interface{}{
				"state":         "in-sync",
				"lastChangedAt": lastChangedAt,
				"lastScaleOpAt": lastScaleAt,
			}).Return(redis.NewStatusResult("", fmt.Errorf("Some error in redis")))
			schedulerState := models.NewSchedulerState(name, state, lastChangedAt, lastScaleAt)
			err = controller.SaveSchedulerStateInfo(logger, mr, mockRedisClient, schedulerState)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("Some error in redis"))
		})
	})

	Describe("GetSchedulerStateInfo", func() {
		It("should succeed", func() {
			name := "controller-name"
			state := "in-sync"
			lastChangedAt := time.Now().Unix()
			lastScaleAt := time.Now().Unix()
			mockRedisClient.EXPECT().HMSet(name, map[string]interface{}{
				"state":         "in-sync",
				"lastChangedAt": lastChangedAt,
				"lastScaleOpAt": lastScaleAt,
			}).Return(redis.NewStatusResult("OK", nil))
			schedulerState := models.NewSchedulerState(name, state, lastChangedAt, lastScaleAt)
			err = controller.SaveSchedulerStateInfo(logger, mr, mockRedisClient, schedulerState)
			Expect(err).NotTo(HaveOccurred())

			mockRedisClient.EXPECT().HGetAll(name).Return(redis.NewStringStringMapResult(map[string]string{
				"state":         state,
				"lastChangedAt": strconv.Itoa(int(lastChangedAt)),
				"lastScaleOpAt": strconv.Itoa(int(lastScaleAt)),
			}, nil))
			retrievedSchedulerState, err := controller.GetSchedulerStateInfo(logger, mr, mockRedisClient, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(retrievedSchedulerState).To(Equal(schedulerState))
		})

		It("should fail if error in redis", func() {
			name := "controller-name"
			state := "in-sync"
			lastChangedAt := time.Now().Unix()
			lastScaleAt := time.Now().Unix()
			mockRedisClient.EXPECT().TxPipeline()
			mockRedisClient.EXPECT().HMSet(name, map[string]interface{}{
				"state":         "in-sync",
				"lastChangedAt": lastChangedAt,
				"lastScaleOpAt": lastScaleAt,
			}).Return(redis.NewStatusResult("", fmt.Errorf("Some error in redis")))
			schedulerState := models.NewSchedulerState(name, state, lastChangedAt, lastScaleAt)
			err = controller.SaveSchedulerStateInfo(logger, mr, mockRedisClient, schedulerState)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("Some error in redis"))
		})
	})
})
