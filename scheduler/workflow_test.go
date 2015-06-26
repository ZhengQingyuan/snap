package scheduler

import (
	"os"
	"path"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/intelsdi-x/pulse/control"
	"github.com/intelsdi-x/pulse/core/cdata"
	"github.com/intelsdi-x/pulse/pkg/schedule"
	"github.com/intelsdi-x/pulse/scheduler/wmap"
)

var (
	PulsePath = os.Getenv("PULSE_PATH")
)

type MockMetricType struct {
	namespace []string
}

func (m MockMetricType) Namespace() []string {
	return m.namespace
}

func (m MockMetricType) LastAdvertisedTime() time.Time {
	return time.Now()
}

func (m MockMetricType) Version() int {
	return 1
}

func (m MockMetricType) Config() *cdata.ConfigDataNode {
	return nil
}

func (m MockMetricType) Data() interface{} {
	return nil
}

func TestCollectPublishWorkflow(t *testing.T) {
	Convey("Given a started plugin control", t, func() {
		// logger.SetLevel(logger.DebugLevel)
		// logPath := "/tmp"
		// file, err := os.OpenFile(fmt.Sprintf("%s/pulse.log", logPath), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		// if err != nil {
		// 	logger.Error("main", fmt.Sprintf("bad log path(%s) - %s\n", logPath, err.Error()))
		// }
		// defer file.Close()
		// logger.Output = file

		c := control.New()
		c.Start()
		s := New()
		s.SetMetricManager(c)
		Convey("Start a collector and publisher plugin", func() {
			_, err := c.Load(path.Join(PulsePath, "plugin", "collector", "pulse-collector-dummy2"))
			So(err, ShouldBeNil)
			_, err = c.Load(path.Join(PulsePath, "plugin", "publisher", "pulse-publisher-file"))
			So(err, ShouldBeNil)
			_, err = c.Load(path.Join(PulsePath, "plugin", "processor", "pulse-processor-movingaverage"))
			So(err, ShouldBeNil)
			time.Sleep(100 * time.Millisecond)

			metrics, err2 := c.MetricCatalog()
			So(err2, ShouldBeNil)
			So(metrics, ShouldNotBeEmpty)

			w := wmap.NewWorkflowMap()
			w.CollectNode.AddMetric("/intel/dummy/foo", 1)
			w.CollectNode.AddConfigItem("/intel/dummy/foo", "password", "secret")

			pu := wmap.NewPublishNode("file", 1)
			pu.AddConfigItem("file", "/tmp/pulse-TestCollectPublishWorkflow.out")

			pr := wmap.NewProcessNode("movingaverage", 1)
			pr.AddConfigItem("MovingAvgBufLength", 20)
			config2, err3 := pr.GetConfigNode()
			So(err3, ShouldBeNil)
			c.SubscribeProcessor("movingaverage", 1, config2.Table())
			time.Sleep(100 * time.Millisecond)

			pr.Add(pu)
			w.CollectNode.Add(pr)

			Convey("Start scheduler", func() {
				err := s.Start()
				So(err, ShouldBeNil)
				Convey("Create task", func() {
					t, err := s.CreateTask(schedule.NewSimpleSchedule(time.Millisecond*500), w)
					So(err.Errors(), ShouldBeEmpty)
					So(t, ShouldNotBeNil)
					t.(*task).Spin()
					time.Sleep(3 * time.Second)
				})
			})
		})
	})
}
