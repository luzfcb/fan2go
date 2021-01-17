/*
 * fan2go
 * Copyright (c) 2019. Markus Ressel
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at ydour option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */
package main

import (
	"errors"
	"fan2go/config"
	"fan2go/persistence"
	"fan2go/util"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/asecurityteam/rolling"
	"github.com/fsnotify/fsnotify"
	//"github.com/markusressel/fan2go/cmd"
	"log"
	"os"
	"path/filepath"
)

const (
	MaxPwmValue   = 255
	MinPwmValue   = 0
	BucketFans    = "fans"
	BucketSensors = "sensors"
)

type Controller struct {
	name     string
	dType    string
	modalias string
	platform string
	path     string
	fans     []*Fan
	sensors  []*Sensor
}

type Fan struct {
	name      string
	index     int
	rpmInput  string
	pwmOutput string
	config    *config.FanConfig
	startPwm  int // lowest PWM value where the fans are still spinning
	maxPwm    int // highest PWM value that yields an RPM increase
}

type Sensor struct {
	name   string
	index  int
	input  string
	config *config.SensorConfig
}

var (
	Controllers []Controller
	SensorMap   = map[string]*Sensor{}
	// map of sensor ids -> sensor value rolling window
	SensorValueArrayMap = map[string]*rolling.PointPolicy{}
	// map fan id -> pwm -> rpm value rolling window
	FanCurveMap = map[string]*map[int]*rolling.PointPolicy{}
)

func main() {
	// TODO: maybe it is possible without root by providing permissions?
	if getProcessOwner() != "root" {
		log.Fatalf("Fan control requires root access, please run fan2go as root")
	}

	defer persistence.Open().Close()

	// TODO: cmd line parameters
	//cmd.Execute()

	// === Detect devices ===
	controllers, err := findControllers()
	if err != nil {
		log.Fatalf("Error detecting devices: %s", err.Error())
	}
	Controllers = controllers
	mapConfigToControllers()

	// === Print detected devices ===
	log.Printf("Detected Devices:")
	printDeviceStatus(Controllers)

	// TODO: measure fan curves / use realtime measurements to update the curve?
	// TODO: save reference fan curves in db

	// === start sensor monitoring
	// TODO: use multiple monitoring threads(?)
	// TODO: only monitor configured sensors
	go monitor()

	// wait a bit to gather monitoring data
	time.Sleep(2 * time.Second)

	// === start fan controllers
	// run one goroutine for each fan
	count := 0
	for _, controller := range Controllers {
		for _, fan := range controller.fans {
			if fan.config == nil {
				// this fan is not configured, ignore it
				log.Printf("Ignoring unconfigured fan: %s", fan.pwmOutput)
				continue
			}

			go fanController(fan)
			count++
		}
	}

	if count == 0 {
		log.Fatal("No valid fan configurations, exiting.")
	}

	// wait forever
	select {}
}

// Map detect devices to configuration values
func mapConfigToControllers() {
	for _, controller := range Controllers {
		// match fan and fan config entries
		for _, fan := range controller.fans {
			fanConfig := findFanConfig(controller, *fan)
			if fanConfig != nil {
				fan.config = fanConfig
			}
		}
		// match sensor and sensor config entries
		for _, sensor := range controller.sensors {
			sensorConfig := findSensorConfig(controller, *sensor)
			if sensorConfig != nil {
				sensor.config = sensorConfig

				SensorMap[sensorConfig.Id] = sensor
				// initialize arrays for storing temps
				pointWindow := rolling.NewPointPolicy(rolling.NewWindow(config.CurrentConfig.TempRollingWindowSize))
				SensorValueArrayMap[sensor.input] = pointWindow
				currentValue, err := util.ReadIntFromFile(sensor.input)
				if err != nil {
					currentValue = 50000
				}
				for i := 0; i < config.CurrentConfig.TempRollingWindowSize; i++ {
					pointWindow.Append(float64(currentValue))
				}
			}
		}
	}
}

func getProcessOwner() string {
	stdout, err := exec.Command("ps", "-o", "user=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
	return strings.TrimSpace(string(stdout))
}

// goroutine to monitor temp and fan sensors
func monitor() {
	go startSensorWatcher()

	// TODO: seems like its not possible to watch for changes on temp and rpm files using inotify :(
	//for _, device := range Controllers {
	//	for _, fan := range device.fans {
	//		watcher, err := startFanFsWatcher(*fan)
	//		if err != nil {
	//			log.Print(err.Error())
	//		} else {
	//			defer watcher.Close()
	//		}
	//	}
	//}

	// wait forever
	select {}
}

func startFanFsWatcher(fan *Fan) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					err := updateFan(fan)
					if err != nil {
						log.Print(err.Error())
					}
					key := fmt.Sprintf("%s_pwm", fan.name)
					newValue, _ := persistence.ReadInt(BucketFans, key)
					log.Printf("%s PWM: %d", fan.name, newValue)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)
			}
		}
	}()

	err = watcher.Add(fan.rpmInput)
	err = watcher.Add(fan.pwmOutput)
	if err != nil {
		log.Fatal(err.Error())
	}

	return watcher, err
}

func updateFan(fan *Fan) (err error) {
	pwmValue := getPwm(fan)
	rpmValue, err := util.ReadIntFromFile(fan.rpmInput)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%s_pwm", fan.name)
	err = persistence.StoreInt(BucketFans, key, pwmValue)
	if err != nil {
		return err
	}
	key = fmt.Sprintf("%s_rpm", fan.name)
	err = persistence.StoreInt(BucketFans, key, rpmValue)
	return err
}

func startSensorWatcher() {
	// update RPM and Temps at different rates
	tempTick := time.Tick(config.CurrentConfig.TempSensorPollingRate)
	rpmTick := time.Tick(config.CurrentConfig.RpmPollingRate)
	for {
		select {
		case <-tempTick:
			measureTempSensors()
		case <-rpmTick:
			measureRpmSensors()
		}
	}
}

func measureRpmSensors() {
	for _, controller := range Controllers {
		for _, fan := range controller.fans {
			err := measureRpm(fan)
			if err != nil {
				log.Printf("Error measuring RPM: %s", err.Error())
			}
		}
	}
}

// read the current value of a fan RPM sensor and append it to the moving window
func measureRpm(fan *Fan) (err error) {
	pwm := getPwm(fan)
	rpm := getRpm(fan)

	pwmRpmMap, ok := FanCurveMap[fan.rpmInput]
	if !ok {
		// create map for the current fan
		pwmRpmMap = &map[int]*rolling.PointPolicy{}
		FanCurveMap[fan.rpmInput] = pwmRpmMap
	}
	pointWindow, ok := (*pwmRpmMap)[pwm]
	if !ok {
		// create rolling window for current pwm value
		pointWindow = rolling.NewPointPolicy(rolling.NewWindow(config.CurrentConfig.RpmRollingWindowSize))
		(*pwmRpmMap)[pwm] = pointWindow
	}
	pointWindow.Append(float64(rpm))

	return persistence.StoreInt(BucketSensors, fan.rpmInput, rpm)
}

func measureTempSensors() {
	for _, controller := range Controllers {
		for _, sensor := range controller.sensors {
			if _, ok := SensorValueArrayMap[sensor.input]; ok {
				err := updateSensor(*sensor)
				if err != nil {
					log.Fatal(err)
				}
			}
		}
	}
}

func updatePwmBoundaries(fan *Fan) {
	startPwm := 255
	maxPwm := 255
	pwmRpmMap, ok := FanCurveMap[fan.rpmInput]
	if !ok {
		// we have no data yet
		startPwm = 0
	} else {
		// get pwm keys that we have data for
		keys := make([]int, len(*pwmRpmMap))
		i := 0
		for k := range *pwmRpmMap {
			keys[i] = k
			i++
		}
		// sort them increasing
		sort.Ints(keys)

		maxRpm := 0
		for _, pwm := range keys {
			window := (*pwmRpmMap)[pwm]
			avgRpm := int(window.Reduce(rolling.Avg))

			if avgRpm > maxRpm {
				maxRpm = avgRpm
				maxPwm = pwm
			}

			if avgRpm > 0 && pwm < startPwm {
				startPwm = pwm
			}
		}
	}

	if fan.startPwm != startPwm {
		log.Printf("Start PWM of %s: %d", fan.rpmInput, startPwm)
		fan.startPwm = startPwm
	}
	if fan.maxPwm != maxPwm {
		log.Printf("Max PWM of %s: %d", fan.rpmInput, startPwm)
		fan.maxPwm = maxPwm
	}
}

// read the current value of a sensor and append it to the moving window
func updateSensor(sensor Sensor) (err error) {
	value, err := util.ReadIntFromFile(sensor.input)
	if err != nil {
		return err
	}

	values := SensorValueArrayMap[sensor.input]
	values.Append(float64(value))
	if value > sensor.config.Max {
		// if the value is higher than the specified max temperature,
		// insert the value twice into the moving window,
		// to give it a bigger impact
		values.Append(float64(value))
	}

	return persistence.StoreInt(BucketSensors, sensor.input, value)
}

// goroutine to continuously adjust the speed of a fan
func fanController(fan *Fan) {
	err := setPwmEnabled(*fan, 1)
	if err != nil {
		err = setPwmEnabled(*fan, 0)
		if err != nil {
			log.Printf("Could not enable fan control on %s", fan.name)
			return
		}
	}

	// TODO: check if this fan is "new"
	runInitializationSequence(fan)
	// TODO: read fan data from database and attach it to the fan object

	t := time.Tick(config.CurrentConfig.ControllerAdjustmentTickRate)
	for {
		select {
		case <-t:
			setOptimalFanSpeed(fan)
		}
	}
}

// runs an initialization sequence for the given fan
// to determine an estimation of its fan curve
func runInitializationSequence(fan *Fan) {
	log.Printf("Running initialization sequence for %s", fan.config.Id)
	for pwm := 0; pwm < MaxPwmValue; pwm++ {
		// set a pwm
		err := util.WriteIntToFile(pwm, fan.pwmOutput)
		if err != nil {
			log.Fatalf("Unable to run initialization sequence on %s: %s", fan.config.Id, err.Error())
		}

		if pwm == 0 {
			// wait an additional 2 seconds, to make sure the fans
			// have time to spin down even from max speed to 0
			time.Sleep(3 * time.Second)
		}

		// TODO:
		// on some fans it is not possible to use the full pwm of 0..255
		// so we try what values work and save them for later

		// wait a bit to allow the fan speed to settle
		// since most sensors are update only each second,
		// we wait a second + a bit, to make sure we get
		// the most recent measurement
		time.Sleep(500 * time.Millisecond) // TODO: use 1s+ here

		log.Printf("Measuring RPM of  %s at PWM: %d", fan.config.Id, pwm)
		for i := 0; i < config.CurrentConfig.RpmRollingWindowSize; i++ {
			// update rpm curve
			err = measureRpm(fan)
			if err != nil {
				log.Fatalf("Unable to update fan curve data on %s: %s", fan.config.Id, err.Error())
			}
		}
	}

	updatePwmBoundaries(fan)

	// TODO: save this data to the database
}

func findFanConfig(controller Controller, fan Fan) (fanConfig *config.FanConfig) {
	for _, fanConfig := range config.CurrentConfig.Fans {
		if controller.platform == fanConfig.Platform &&
			fan.index == fanConfig.Fan {
			return &fanConfig
		}
	}
	return nil
}

func findSensorConfig(controller Controller, sensor Sensor) (sensorConfig *config.SensorConfig) {
	for _, sensorConfig := range config.CurrentConfig.Sensors {
		if controller.platform == sensorConfig.Platform &&
			sensor.index == sensorConfig.Index {
			return &sensorConfig
		}
	}
	return nil
}

// calculates optimal fan speeds for all given devices
func setOptimalFanSpeed(fan *Fan) {
	target := calculateTargetSpeed(fan)
	err := setPwm(fan, target)
	if err != nil {
		log.Printf("Error setting %s/%d: %s", fan.name, fan.index, err.Error())
	}
}

// calculates the target speed for a given device output
func calculateTargetSpeed(fan *Fan) int {
	sensor := SensorMap[fan.config.Sensor]
	minTemp := sensor.config.Min * 1000 // degree to milli-degree
	maxTemp := sensor.config.Max * 1000

	var avgTemp int
	temps := SensorValueArrayMap[sensor.input]
	avgTemp = int(temps.Reduce(rolling.Avg))

	//log.Printf("Avg temp of %s: %d", sensor.name, avgTemp)

	if avgTemp >= maxTemp {
		// full throttle if max temp is reached
		return 255
	} else if avgTemp <= minTemp {
		// turn fan off if at/below min temp
		return 0
	}

	ratio := (float64(avgTemp) - float64(minTemp)) / (float64(maxTemp) - float64(minTemp))
	return int(ratio * 255)

	// Toggling between off and "full on" for testing
	//pwm := getPwm(fan)
	//if pwm < 255 {
	//	return 255
	//}
	//
	//return 1

	//return rand.Intn(getMaxPwmValue(fan))
}

// Finds controllers and fans
func findControllers() (controllers []Controller, err error) {
	hwmonDevices := util.FindHwmonDevicePaths()
	i2cDevices := util.FindI2cDevicePaths()
	allDevices := append(hwmonDevices, i2cDevices...)

	platformRegex := regexp.MustCompile(".*/platform/{}/.*")

	for _, devicePath := range allDevices {
		name := util.GetDeviceName(devicePath)
		dType := util.GetDeviceType(devicePath)
		modalias := util.GetDeviceModalias(devicePath)
		platform := platformRegex.FindString(devicePath)
		if len(platform) <= 0 {
			platform = name
		}

		fans := createFans(devicePath)
		sensors := createSensors(devicePath)

		if len(fans) <= 0 && len(sensors) <= 0 {
			continue
		}

		controller := Controller{
			name:     name,
			dType:    dType,
			modalias: modalias,
			platform: platform,
			path:     devicePath,
			fans:     fans,
			sensors:  sensors,
		}
		controllers = append(controllers, controller)
	}

	return controllers, err
}

// creates fan objects for the given device path
func createFans(devicePath string) []*Fan {
	var fans []*Fan

	inputs := util.FindFilesMatching(devicePath, "^fan[1-9]_input$")
	outputs := util.FindFilesMatching(devicePath, "^pwm[1-9]$")

	for idx, output := range outputs {
		_, file := filepath.Split(output)

		index, err := strconv.Atoi(file[len(file)-1:])
		if err != nil {
			log.Fatal(err)
		}

		fans = append(fans, &Fan{
			name:      file,
			index:     index,
			pwmOutput: output,
			rpmInput:  inputs[idx],
			startPwm:  0,
			maxPwm:    255,
		})
	}

	return fans
}

// creates sensor objects for the given device path
func createSensors(devicePath string) []*Sensor {
	var sensors []*Sensor

	inputs := util.FindFilesMatching(devicePath, "^temp[1-9]_input$")

	for _, input := range inputs {
		_, file := filepath.Split(input)

		index, err := strconv.Atoi(string(file[4]))
		if err != nil {
			log.Fatal(err)
		}

		sensors = append(sensors, &Sensor{
			name:  file,
			index: index,
			input: input,
		})
	}

	return sensors
}

// checks if the given output is in auto mode
func isPwmAuto(outputPath string) (bool, error) {
	pwmEnabledFilePath := outputPath + "_enable"

	if _, err := os.Stat(pwmEnabledFilePath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		panic(err)
	}

	value, err := util.ReadIntFromFile(pwmEnabledFilePath)
	if err != nil {
		return false, err
	}
	return value > 1, nil
}

// Writes the given value to pwmX_enable
// Possible values (unsure if these are true for all scenarios):
// 0 - no control (results in max speed)
// 1 - manual pwm control
// 2 - motherboard pwm control
func setPwmEnabled(fan Fan, value int) (err error) {
	pwmEnabledFilePath := fan.pwmOutput + "_enable"
	err = util.WriteIntToFile(value, pwmEnabledFilePath)
	if err == nil {
		value, err := util.ReadIntFromFile(pwmEnabledFilePath)
		if err != nil || value != value {
			return errors.New(fmt.Sprintf("PWM mode stuck to %d", value))
		}
	}
	return err
}

// get the pwmX_enabled value of a fan
func getPwmEnabled(fan Fan) (int, error) {
	pwmEnabledFilePath := fan.pwmOutput + "_enable"
	return util.ReadIntFromFile(pwmEnabledFilePath)
}

// get the maximum valid pwm value of a fan
func getMaxPwmValue(fan *Fan) (result int) {
	// TODO: load this from persistence

	return fan.maxPwm
	//
	//key := fmt.Sprintf("%s_pwm_max", fan.name)
	//result, err := readInt(BucketFans, key)
	//if err != nil {
	//	result = MaxPwmValue
	//}
	//return result
}

// get the minimum valid pwm value of a fan
func getMinPwmValue(fan *Fan) (result int) {
	// if the fan is never supposed to stop,
	// use the lowest pwm value where the fan is still spinning
	if fan.config.NeverStop {
		return fan.startPwm
	}

	// get the minimum possible pwm value for this fan
	key := fmt.Sprintf("%s_pwm_min", fan.name)
	result, err := persistence.ReadInt(BucketFans, key)
	if err != nil {
		result = MinPwmValue
	}

	return result
}

// get the pwm speed of a fan (0..255)
func getPwm(fan *Fan) int {
	value, err := util.ReadIntFromFile(fan.pwmOutput)
	if err != nil {
		return MinPwmValue
	}
	return value
}

// set the pwm speed of a fan to the specified value (0..255)
func setPwm(fan *Fan, pwm int) (err error) {
	// ensure target value is within bounds of possible values
	if pwm > MaxPwmValue {
		pwm = MaxPwmValue
	} else if pwm < MinPwmValue {
		pwm = MinPwmValue
	}

	// map the target value to the possible range of this fan
	maxPwm := getMaxPwmValue(fan)
	minPwm := getMinPwmValue(fan)

	target := minPwm + int((float64(pwm)/MaxPwmValue)*(float64(maxPwm)-float64(minPwm)))

	// TODO: map target pwm to fancurve?

	current := getPwm(fan)
	if target == current {
		return nil
	}
	log.Printf("Setting %s to %d (mapped: %d) ...", fan.name, pwm, target)
	return util.WriteIntToFile(target, fan.pwmOutput)
}

// get the rpm value of a fan
func getRpm(fan *Fan) int {
	value, err := util.ReadIntFromFile(fan.rpmInput)
	if err != nil {
		return 0
	}
	return value
}

// ===== Console Output =====

func printDeviceStatus(devices []Controller) {
	for _, device := range devices {
		log.Printf("Controller: %s", device.name)
		for _, fan := range device.fans {
			pwm := getPwm(fan)
			rpm := getRpm(fan)
			isAuto, _ := isPwmAuto(device.path)
			log.Printf("Fan %d (%s): RPM: %d PWM: %d Auto: %v", fan.index, fan.name, rpm, pwm, isAuto)
		}

		for _, sensor := range device.sensors {
			value, _ := util.ReadIntFromFile(sensor.input)
			log.Printf("Sensor %d (%s): %d", sensor.index, sensor.name, value)
		}
	}
}
