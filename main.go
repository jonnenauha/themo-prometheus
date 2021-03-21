package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/viper"

	logrus "github.com/sirupsen/logrus"
)

var (
	Log            = logrus.New()
	TimeFormat     = "02.01.2006 15:04:05.000"
	TimeFormatZone = TimeFormat + " -0700"
)

func main() {
	viper.SetConfigName("config")
	viper.AddConfigPath("./")
	if err := viper.ReadInConfig(); err != nil {
		Log.Fatalf("failed to read config file: %s", err)
	}

	Log.SetOutput(os.Stdout)
	Log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:          true,
		TimestampFormat:        TimeFormat,
		DisableLevelTruncation: false,
		PadLevelText:           true,
	})
	l := logrus.InfoLevel
	if viper.GetString("log_level") == "debug" {
		l = logrus.DebugLevel
	}
	Log.SetLevel(l)

	c, err := NewThemoClient(viper.GetString("themo.username"), viper.GetString("themo.password"))
	if err != nil {
		Log.Fatal(err.Error())
	}
	if err = c.Auth(); err != nil {
		Log.Fatal("auth failed: ", err.Error())
	}
	if err = c.Devices(); err != nil {
		Log.Fatal("devices failed: ", err.Error())
	}

	Log.Printf("themo.update_interval %s", c.UpdateInterval().String())

	go func() {
		time.Sleep(time.Second * 1)
		for {
			var (
				next_update_at      = time.Time{}
				next_update_devices = []*Device{}
			)

			for _, device := range c.devices {
				if !device.State.Timestamp.IsZero() {
					update_at := device.State.Timestamp.Add(c.UpdateInterval())
					if next_update_at.IsZero() || update_at.Before(next_update_at) {
						next_update_at = update_at
					}
				}
			}

			// in the past
			if !next_update_at.IsZero() && next_update_at.Before(time.Now()) {
				next_update_at = time.Now().Add(c.UpdateInterval())
			}

			// look for all devices that hit the next update
			next_update_device_names := []string{}
			for _, device := range c.devices {
				if !device.State.Timestamp.IsZero() {
					update_at := device.State.Timestamp.Add(c.UpdateInterval()).Truncate(time.Minute)
					if update_at.Before(next_update_at) || update_at.Equal(next_update_at) {
						next_update_devices = append(next_update_devices, device)
						next_update_device_names = append(next_update_device_names, device.DeviceName)
					}
				}
			}
			next_update_in := next_update_at.Sub(time.Now()).Round(time.Second)
			if next_update_in < 0 {
				next_update_in = time.Duration(0)
			}

			if len(next_update_device_names) > 0 {
				Log.
					WithField("at", next_update_at.Format(TimeFormatZone)).
					WithField("devices", strings.Join(next_update_device_names, ", ")).
					Printf("next update in %s", next_update_in)
			} else if !next_update_at.IsZero() {
				Log.
					WithField("at", next_update_at.Format(TimeFormatZone)).
					Printf("next update in %s", next_update_in)
			} else {
				Log.Printf("next update in %s", next_update_in)
			}

			select {
			case _ = <-time.After(next_update_in):
				if len(next_update_devices) == 0 {
					next_update_devices = c.devices
				}
				for _, d := range next_update_devices {
					if err := c.UpdateDeviceState(d); err != nil {
						Log.Errorf("Failed to update device: %s", err.Error())
						continue
					}
				}
			}

			fmt.Printf("\n")
		}
	}()

	registry := prometheus.NewRegistry()
	if err := registry.Register(c); err != nil {
		Log.Fatalf("registry.Register failed: %s", err.Error())
	}
	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorLog: Log,
	})
	http.Handle("/metrics", handler)

	listen := viper.GetString("http.listen")
	Log.WithField("type", "http").Println("listen", listen)
	fmt.Printf("\n")

	http.ListenAndServe(listen, nil)
}

type ThemoClient struct {
	sync.RWMutex

	client *http.Client

	username string
	password string

	auth *tokenResp

	devices []*Device
}

func NewThemoClient(username, password string) (*ThemoClient, error) {
	if len(username) == 0 {
		return nil, errors.New("themo.username not defined")
	}
	if len(password) == 0 {
		return nil, errors.New("themo.password not defined")
	}
	c := &ThemoClient{
		client:   http.DefaultClient,
		username: username,
		password: password,
	}
	return c, nil
}

func (c *ThemoClient) UpdateInterval() time.Duration {
	update_interval := viper.GetDuration("themo.update_interval")
	if update_interval == 0 {
		update_interval = time.Minute * 30
	}
	return update_interval
}

// Implements prometheus.Collector
func (c *ThemoClient) Describe(ch chan<- *prometheus.Desc) {
	start := time.Now()

	Log.Debugf("prometheus.Collector.Describe  %s", time.Now().Sub(start))
}

// Implements prometheus.Collector
func (c *ThemoClient) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()

	c.Lock()
	defer c.Unlock()

	desc_updated := prometheus.NewDesc("themo_sensor_updated", "updated timestamp in UTC epoch",
		[]string{"device", "device_id", "device_type"},
		nil,
	)

	metric_fqname := func(prop string) string {
		return fmt.Sprintf("themo_sensor_%s", strings.ToLower(strings.TrimSpace(prop)))
	}

	for _, device := range c.devices {
		if device.State.Timestamp.IsZero() {
			continue
		}

		id_str, device_id_str := strconv.Itoa(device.ID), strconv.Itoa(device.DeviceType.ID)

		for prop, value := range map[string]float64{
			"FT":     device.State.FT,
			"FTS":    device.State.FTS,
			"Info":   device.State.Info,
			"Lights": device.State.Lights,
			"Locks":  device.State.Lock,
			"MP":     device.State.MP,
			"MT":     device.State.MT,
			"Ot":     device.State.Ot,
			"RT":     device.State.RT,
		} {
			desc := prometheus.NewDesc(metric_fqname(prop), fmt.Sprintf("Device state %s", prop),
				[]string{"device", "device_id", "device_type"},
				nil,
			)
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value,
				device.DeviceName, id_str, device_id_str,
			)
		}
		for prop, value := range map[string]bool{
			"Data":   device.State.Data,
			"NV":     device.State.NV,
			"Online": device.State.Online,
			"Power":  device.State.Power,
		} {
			desc := prometheus.NewDesc(metric_fqname(prop), fmt.Sprintf("Device state boolean %s. 0 = false 1 = true.", prop),
				[]string{"device", "device_id", "device_type"},
				nil,
			)
			value_num := 0
			if value {
				value_num = 1
			}
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(value_num),
				device.DeviceName, id_str, device_id_str,
			)
		}

		ch <- prometheus.MustNewConstMetric(desc_updated, prometheus.CounterValue, float64(device.State.Timestamp.UTC().Unix()),
			device.DeviceName, id_str, device_id_str,
		)
	}

	Log.Debugf("prometheus.Collector.Collect   %s", time.Now().Sub(start))
}

func (c *ThemoClient) URL(path string) string {
	if strings.HasPrefix(path, "/") {
		path = path[1:]
	}
	return fmt.Sprintf("https://app.themo.io/%s", path)
}

func (c *ThemoClient) Auth() error {
	c.Lock()
	defer c.Unlock()

	body := url.Values{}
	body.Set("grant_type", "password")
	body.Set("username", c.username)
	body.Set("password", c.password)

	req, err := http.NewRequest("POST", c.URL("/token"), strings.NewReader(body.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := &tokenResp{
		Executed: time.Now(),
	}
	if err = c.respJSON(req, resp); err != nil {
		return err
	}
	if len(resp.AccessToken) == 0 {
		return fmt.Errorf("received empty auth token")
	} else if resp.ExpiresIn == 0 {
		return fmt.Errorf("received zero auth expiration")
	}

	c.auth = resp

	Log.WithFields(logrus.Fields{
		"access_token": resp.AccessToken[0:8] + "...",
		"expires_in":   resp.ExpiresIn,
	}).Info("autenticated")

	return nil
}

type Device struct {
	ClientID       int    `json:"ClientID"`
	ClientFullname string `json:"Client_FullName"`
	Country        string `json:"Country"`
	CountryShort   string `json:"CountryShort"`
	// Not read, I don't know what this is used for and if harmful to debug log etc.
	//DeviceAuth       string         `json:"-"`
	DeviceID         string         `json:"DeviceID"`
	DeviceLocation   Devicelocation `json:"DeviceLocation"`
	DeviceName       string         `json:"DeviceName"`
	DeviceType       Devicetype     `json:"DeviceType"`
	DeviceTypeID     int            `json:"DeviceTypeID"`
	EnvironmentName  string         `json:"EnvironmentName"`
	ID               int            `json:"ID"`
	Spotoptimization bool           `json:"SpotOptimization"`

	State DeviceState `json:"State"`
}

type Devicelocation struct {
	ID            int     `json:"ID"`
	Lat           float64 `json:"Lat"`
	Lng           float64 `json:"Lng"`
	RawTimeOffset int     `json:"RawTimeOffset"`
}

type ParentType struct {
	Description string `json:"Description"`
	ID          int    `json:"ID"`
	Name        string `json:"Name"`
}

type Devicetype struct {
	Description  string     `json:"Description"`
	ID           int        `json:"ID"`
	Name         string     `json:"Name"`
	ParentType   ParentType `json:"ParentType"`
	ParentTypeID int        `json:"ParentTypeID"`
}

func (c *ThemoClient) Devices() error {
	c.Lock()
	defer c.Unlock()

	query := url.Values{}
	query.Set("clientID", "218") // @todo
	query.Set("state", "false")
	query.Set("page", "0")
	query.Set("pageSize", "-1")

	req, err := http.NewRequest("GET", c.URL("/api/devices?"+query.Encode()), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.auth.AccessToken)

	devices := struct {
		Devices []*Device
	}{
		Devices: []*Device{},
	}
	if err = c.respJSON(req, &devices); err != nil {
		return err
	}
	c.devices = devices.Devices

	if Log.IsLevelEnabled(logrus.DebugLevel) {
		for _, d := range c.devices {
			b, _ := json.MarshalIndent(d, "", "  ")
			Log.Debugln("Found device:", string(b))
		}
	}

	return nil
}

type DeviceState struct {
	Data      bool      `json:"Data"`
	FT        float64   `json:"FT"`
	FTS       float64   `json:"FTS"`
	Info      float64   `json:"Info"`
	Lights    float64   `json:"Lights"`
	Lock      float64   `json:"Lock"`
	Mode      string    `json:"Mode"`
	MP        float64   `json:"MP"`
	MT        float64   `json:"MT"`
	NV        bool      `json:"NV"`
	Online    bool      `json:"Online"`
	Ot        float64   `json:"OT"`
	Power     bool      `json:"Power"`
	RT        float64   `json:"RT"`
	SW        string    `json:"SW"`
	Timestamp time.Time `json:"Timestamp"`
}

func (c *ThemoClient) UpdateDeviceState(device *Device) error {
	c.Lock()
	defer c.Unlock()

	started := time.Now()
	req, err := http.NewRequest("GET", c.URL(fmt.Sprintf("/api/devices/%d/state", device.ID)), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.auth.AccessToken)
	if err = c.respJSON(req, &device.State); err != nil {
		return err
	}

	zone_id := fmt.Sprintf("themo.device.%d", device.ID)
	zone := time.FixedZone(zone_id, device.DeviceLocation.RawTimeOffset)

	// apply time offset
	device.State.Timestamp = device.State.Timestamp.In(zone)

	Log.WithFields(logrus.Fields{
		"device":  device.DeviceName,
		"updated": device.State.Timestamp.Format(TimeFormatZone),
		"spent":   time.Now().Sub(started),
	}).Printf("RT: %.1f FT: %.1f FTS: %.1f Info: %.1f",
		device.State.RT, device.State.FT,
		device.State.FTS, device.State.Info,
	)

	if Log.IsLevelEnabled(logrus.DebugLevel) {
		b, _ := json.MarshalIndent(device.State, "", "  ")
		Log.Debugln(string(b))
	}

	return nil
}

func (c *ThemoClient) respJSON(req *http.Request, dest interface{}) error {
	if Log.IsLevelEnabled(logrus.DebugLevel) {
		fields := logrus.Fields{
			"type": "http",
		}
		for k, v := range req.URL.Query() {
			if len(v) == 1 {
				fields[k] = v[0]
			} else {
				fields[k] = strings.Join(v, ",")
			}
		}
		Log.WithFields(fields).Debugln(req.URL.Path)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	} else if resp.Body == nil {
		return fmt.Errorf("nil body")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(dest)
}

type tokenResp struct {
	// server response
	AccessToken string `json:"access_token"`

	// @todo figure out what this is. API is giving back values like '31504463999' which is not epoch
	// its the time in seconds when it expires, seems weirdly high number.
	ExpiresIn int `json:"expires_in"`

	// local
	Executed time.Time `json:"executed"`
}

func (auth *tokenResp) expired() bool {
	// @todo see ExpiresIn
	return false
}
