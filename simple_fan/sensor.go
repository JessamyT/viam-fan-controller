package simple_fan

import (
	"context"
	"regexp"
	"sync"
	"time"

	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	viam_utils "go.viam.com/utils"

	"github.com/viam-soleng/viam-fan-controller/utils"
)

var Model = resource.NewModel("viam-soleng", "fan", "simple")
var PrettyName = "Raspberry Pi Clock Sensor"
var Description = "Simple PWM fan controller for Viam"
var Version = utils.Version

type Config struct {
	resource.Named
	mu               sync.RWMutex
	logger           logging.Logger
	cancelCtx        context.Context
	cancelFunc       func()
	monitor          func()
	done             chan bool
	wg               sync.WaitGroup
	FanPin           board.GPIOPin
	Board            *board.Board
	Sensor           sensor.Sensor
	SensorValueField string
	SensorValueRegex *regexp.Regexp
	OnTemperature    float64
	OffTemperature   float64
	OnDelay          int64
	OffDelay         int64
	LastStateChange  time.Time
}

func init() {
	resource.RegisterComponent(
		sensor.API,
		Model,
		resource.Registration[sensor.Sensor, *CloudConfig]{Constructor: NewSensor})
}

func NewSensor(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	logger.Infof("Starting %s %s", PrettyName, Version)
	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	b := Config{
		Named:      conf.ResourceName().AsNamed(),
		logger:     logger,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
		mu:         sync.RWMutex{},
	}

	if err := b.Reconfigure(ctx, deps, conf); err != nil {
		return nil, err
	}
	return &b, nil
}

func (c *Config) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logger.Debugf("Reconfiguring %s", PrettyName)

	// In case the module has changed name
	c.Named = conf.ResourceName().AsNamed()

	newConf, err := resource.NativeConfig[*CloudConfig](conf)
	if err != nil {
		return err
	}

	untypedBoard, err := deps.Lookup(resource.NewName(board.API, newConf.BoardName))
	if err != nil {
		c.logger.Errorf("Error looking up board: %s", err)
		return err
	}

	board := untypedBoard.(board.Board)
	fanPin, err := board.GPIOPinByName(newConf.FanPin)
	if err != nil {
		c.logger.Errorf("Error looking up fan pin: %s", err)
		return err
	}

	untypedSensor, err := deps.Lookup(resource.NewName(sensor.API, newConf.SensorName))
	if err != nil {
		c.logger.Errorf("Error looking up sensor: %s", err)
		return err
	}
	sensor := untypedSensor.(sensor.Sensor)

	c.Named = conf.ResourceName().AsNamed()
	c.Board = &board
	c.FanPin = fanPin
	c.Sensor = sensor
	c.SensorValueField = newConf.SensorValueField
	c.OnTemperature = newConf.OnTemperature
	c.OffTemperature = newConf.OffTemperature
	c.OnDelay = newConf.OnDelay
	c.OffDelay = newConf.OffDelay

	// We might not always get a regex, some sensors just return a number that can be parsed
	if newConf.SensorValueRegex != "" {
		c.SensorValueRegex = regexp.MustCompile(newConf.SensorValueRegex)
	}

	if c.monitor == nil {
		c.monitor = func() {
			ctx := context.Background()
			c.wg.Add(1)
			defer c.wg.Done()
			for {
				select {
				case <-c.done:
					return
				default:
					readings, err := c.Sensor.Readings(ctx, nil)
					if err != nil {
						c.logger.Errorf("Error getting readings from sensor: %s", err)
						break
					}

					currentTemp, err := utils.ParseCurrentTemperatureFromReadings(ctx, readings, c.SensorValueField, c.SensorValueRegex, c.logger)
					if err != nil {
						c.logger.Errorf("Error parsing current temperature: %s", err)
						break
					}

					isRunning, err := c.FanPin.Get(ctx, nil)
					if err != nil {
						c.logger.Errorf("Error getting fan state: %s", err)
						break
					}

					// If the current temp is calling for the fan to be on, and the fan isn't on, and the last state change was long enough ago, turn the fan on
					if currentTemp > c.OnTemperature && !isRunning && c.LastStateChange.Add(time.Duration(c.OnDelay)).UnixMilli() < time.Now().UnixMilli() {
						c.logger.Infof("Turning fan on")
						c.FanPin.Set(ctx, true, nil)
						c.LastStateChange = time.Now()
					}

					// If the current temp is calling for the fan to be off, and the fan is on, and the last state change was long enough ago, turn the fan off
					if currentTemp < c.OffTemperature && isRunning && c.LastStateChange.Add(time.Duration(c.OffDelay)).UnixMilli() < time.Now().UnixMilli() {
						c.logger.Infof("Turning fan off")
						c.FanPin.Set(ctx, false, nil)
						c.LastStateChange = time.Now()
					}
				}

				time.Sleep(100 * time.Millisecond)
			}
		}

		viam_utils.PanicCapturingGo(c.monitor)
	}

	return nil
}

func (c *Config) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	readings, err := c.Sensor.Readings(ctx, nil)
	if err != nil {
		c.logger.Errorf("Error getting readings from sensor: %s", err)
		return nil, err
	}

	currentTemp, err := utils.ParseCurrentTemperatureFromReadings(ctx, readings, c.SensorValueField, c.SensorValueRegex, c.logger)
	if err != nil {
		c.logger.Errorf("Error parsing current temperature: %s", err)
		return nil, err
	}

	isRunning, err := c.FanPin.Get(ctx, nil)
	if err != nil {
		c.logger.Errorf("Error getting fan speed: %s", err)
		return nil, err
	}

	return map[string]interface{}{
		"temperature":    currentTemp,
		"fan_is_running": isRunning,
	}, nil
}

func (c *Config) Close(ctx context.Context) error {
	c.logger.Infof("Shutting down %s", PrettyName)
	c.done <- true
	c.logger.Infof("Notifying monitor to shut down")
	c.wg.Wait()
	c.logger.Info("Monitor shut down")
	return nil
}

func (c *Config) Ready(ctx context.Context, extra map[string]interface{}) (bool, error) {
	return false, nil
}
