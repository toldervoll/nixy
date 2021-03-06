package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/Sirupsen/logrus"
)

type MarathonTasks struct {
	Tasks []struct {
		AppId              string `json:"appId"`
		HealthCheckResults []struct {
			Alive bool `json:"alive"`
		} `json:"healthCheckResults"`
		Host         string  `json:"host"`
		Id           string  `json:"id"`
		Ports        []int64 `json:"ports"`
		ServicePorts []int64 `json:"servicePorts"`
		StagedAt     string  `json:"stagedAt"`
		StartedAt    string  `json:"startedAt"`
		Version      string  `json:"version"`
	} `json:"tasks"`
}

type MarathonApps struct {
	Apps []struct {
		Id              string            `json:"id"`
		Labels          map[string]string `json:"labels"`
		Env             map[string]string `json:"env"`
		HealthChecks    []interface{}     `json:"healthChecks"`
		PortDefinitions []struct {
			Port     int64             `json:"port"`
			Protocol string            `json:"protocol"`
			Labels   map[string]string `json:"labels"`
		} `json:"portDefinitions"`
	} `json:"apps"`
}

func eventStream() {
	go func() {
		client := &http.Client{
			Timeout:   0 * time.Second,
			Transport: tr,
		}
		ticker := time.NewTicker(1 * time.Second)
		for _ = range ticker.C {
			var endpoint string
			for _, es := range health.Endpoints {
				if es.Healthy == true {
					endpoint = es.Endpoint
					break
				}
			}
			if endpoint == "" {
				logger.Error("all endpoints are down")
				continue
			}
			req, err := http.NewRequest("GET", endpoint+"/v2/events", nil)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"error":    err.Error(),
					"endpoint": endpoint,
				}).Error("unable to create event stream request")
				continue
			}
			req.Header.Set("Accept", "text/event-stream")
			if config.User != "" {
				req.SetBasicAuth(config.User, config.Pass)
			}
			// Using new context package from Go 1.7
			ctx, cancel := context.WithCancel(context.TODO())
			// initial request cancellation timer of 15s
			timer := time.AfterFunc(15*time.Second, func() {
				cancel()
				logger.Warn("No data for 15s, event stream request was cancelled")
			})
			req = req.WithContext(ctx)
			resp, err := client.Do(req)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"error":    err.Error(),
					"endpoint": endpoint,
				}).Error("unable to access Marathon event stream")
				// expire request cancellation timer immediately
				timer.Reset(100 * time.Millisecond)
				continue
			}
			reader := bufio.NewReader(resp.Body)
			for {
				// reset request cancellation timer to 15s (should be >10s to avoid unnecessary reconnects
				// since ~10s seems to be the rate for dummy/keepalive events on the marathon event stream
				timer.Reset(15 * time.Second)
				line, err := reader.ReadString('\n')
				if err != nil {
					logger.WithFields(logrus.Fields{
						"error":    err.Error(),
						"endpoint": endpoint,
					}).Error("error reading Marathon event stream")
					resp.Body.Close()
					break
				}
				if !strings.HasPrefix(line, "event: ") {
					continue
				}
				logger.WithFields(logrus.Fields{
					"event":    strings.TrimSpace(line[6:]),
					"endpoint": endpoint,
				}).Info("marathon event received")
				select {
				case eventqueue <- true: // add reload to our queue channel, unless it is full of course.
				default:
					logger.Warn("queue is full")
				}
			}
			resp.Body.Close()
			logger.Warn("event stream connection was closed, re-opening")
		}
	}()
}

func endpointHealth() {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for {
			select {
			case <-ticker.C:
				for i, es := range health.Endpoints {
					client := &http.Client{
						Timeout:   5 * time.Second,
						Transport: tr,
					}
					req, err := http.NewRequest("GET", es.Endpoint+"/ping", nil)
					if err != nil {
						logger.WithFields(logrus.Fields{
							"error":    err.Error(),
							"endpoint": es.Endpoint,
						}).Error("an error occurred creating endpoint health request")
						health.Endpoints[i].Healthy = false
						health.Endpoints[i].Message = err.Error()
						continue
					}
					if config.User != "" {
						req.SetBasicAuth(config.User, config.Pass)
					}
					resp, err := client.Do(req)
					if err != nil {
						logger.WithFields(logrus.Fields{
							"error":    err.Error(),
							"endpoint": es.Endpoint,
						}).Error("endpoint is down")
						health.Endpoints[i].Healthy = false
						health.Endpoints[i].Message = err.Error()
						continue
					}
					resp.Body.Close()
					if resp.StatusCode != 200 {
						logger.WithFields(logrus.Fields{
							"status":   resp.StatusCode,
							"endpoint": es.Endpoint,
						}).Error("endpoint check failed")
						health.Endpoints[i].Healthy = false
						health.Endpoints[i].Message = resp.Status
						continue
					}
					health.Endpoints[i].Healthy = true
					health.Endpoints[i].Message = "OK"
				}
			}
		}
	}()
}

func eventWorker() {
	go func() {
		// a ticker channel to limit reloads to marathon, 1s is enough for now.
		ticker := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-ticker.C:
				<-eventqueue
				start := time.Now()
				err := reload()
				elapsed := time.Since(start)
				if err != nil {
					logger.Error("config update failed")
					go statsCount("reload.failed", 1)
				} else {
					logger.WithFields(logrus.Fields{
						"took": elapsed,
					}).Info("config updated")
					go statsCount("reload.success", 1)
					go statsTiming("reload.time", elapsed)
				}
			}
		}
	}()
}

func fetchApps(jsontasks *MarathonTasks, jsonapps *MarathonApps) error {
	var endpoint string
	for _, es := range health.Endpoints {
		if es.Healthy == true {
			endpoint = es.Endpoint
			break
		}
	}
	if endpoint == "" {
		err := errors.New("all endpoints are down")
		return err
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: tr,
	}
	// take advantage of goroutines and run both reqs concurrent.
	appschn := make(chan error)
	taskschn := make(chan error)
	go func() {
		req, err := http.NewRequest("GET", endpoint+"/v2/tasks", nil)
		if err != nil {
			taskschn <- err
			return
		}
		req.Header.Set("Accept", "application/json")
		if config.User != "" {
			req.SetBasicAuth(config.User, config.Pass)
		}
		resp, err := client.Do(req)
		if err != nil {
			taskschn <- err
			return
		}
		defer resp.Body.Close()
		decoder := json.NewDecoder(resp.Body)
		err = decoder.Decode(&jsontasks)
		if err != nil {
			taskschn <- err
			return
		}
		taskschn <- nil
	}()
	go func() {
		req, err := http.NewRequest("GET", endpoint+"/v2/apps", nil)
		if err != nil {
			appschn <- err
			return
		}
		req.Header.Set("Accept", "application/json")
		if config.User != "" {
			req.SetBasicAuth(config.User, config.Pass)
		}
		resp, err := client.Do(req)
		if err != nil {
			appschn <- err
			return
		}
		defer resp.Body.Close()
		decoder := json.NewDecoder(resp.Body)
		err = decoder.Decode(&jsonapps)
		if err != nil {
			appschn <- err
			return
		}
		appschn <- nil
	}()
	appserr := <-appschn
	taskserr := <-taskschn
	if appserr != nil {
		return appserr
	}
	if taskserr != nil {
		return taskserr
	}
	if len(jsonapps.Apps) == 0 && len(jsontasks.Tasks) == 0 {
		return errors.New("no apps or tasks found")
	}
	return nil
}

func syncApps(jsontasks *MarathonTasks, jsonapps *MarathonApps) {
	config.Lock()
	defer config.Unlock()
	config.Apps = make(map[string]App)
	for _, app := range jsonapps.Apps {
	OUTER:
		for _, task := range jsontasks.Tasks {
			if task.AppId != app.Id {
				continue
			}
			// lets skip tasks that does not expose any ports.
			if len(task.Ports) == 0 {
				continue
			}
			if len(app.HealthChecks) > 0 {
				if len(task.HealthCheckResults) == 0 {
					// this means tasks is being deployed but not yet monitored as alive. Assume down.
					continue
				}
				alive := true
				for _, health := range task.HealthCheckResults {
					// check if health check is alive
					if health.Alive == false {
						alive = false
					}
				}
				if alive != true {
					// at least one health check has failed. Assume down.
					continue
				}
			}
			if _, ok := config.Apps[app.Id]; !ok {
				var newapp = App{}
				if s, ok := app.Labels["subdomain"]; ok {
					hosts := strings.Split(s, " ")
					for _, host := range hosts {
						newapp.Hosts = append(newapp.Hosts, host)
					}
				} else if s, ok := app.Labels["moxy_subdomain"]; ok {
					// to be compatible with moxy
					hosts := strings.Split(s, " ")
					for _, host := range hosts {
						newapp.Hosts = append(newapp.Hosts, host)
					}
				} else {
					// If directories are used lets use them as subdomain dividers.
					// Ex: /project/app becomes app.project
					// Ex: /app becomes just app
					domains := strings.Split(app.Id[1:], "/")
					for i, j := 0, len(domains)-1; i < j; i, j = i+1, j-1 {
						domains[i], domains[j] = domains[j], domains[i]
					}
					host := strings.Join(domains, ".")
					newapp.Hosts = append(newapp.Hosts, host)
				}
				for _, confapp := range config.Apps {
					for _, host := range confapp.Hosts {
						for _, newhost := range newapp.Hosts {
							if newhost == host {
								logger.WithFields(logrus.Fields{
									"app":       app.Id,
									"subdomain": host,
								}).Warn("duplicate subdomain label, ignoring app")
								continue OUTER
							}
						}
					}
				}
				newapp.Labels = app.Labels
				newapp.Env = app.Env
				for _, pds := range app.PortDefinitions {
					pd := PortDefinitions{
						Port:     pds.Port,
						Protocol: pds.Protocol,
						Labels:   pds.Labels,
					}
					newapp.PortDefinitions = append(newapp.PortDefinitions, pd)
				}
				config.Apps[app.Id] = newapp
			}
			newapp := config.Apps[app.Id]
			var newtask = Task{}
			newtask.Host = task.Host
			newtask.Ports = task.Ports
			newtask.ServicePorts = task.ServicePorts
			newtask.StagedAt = task.StagedAt
			newtask.StartedAt = task.StartedAt
			newtask.Version = task.Version
			newapp.Tasks = append(newapp.Tasks, newtask)
			config.Apps[app.Id] = newapp
		}
	}
}

func writeConf() error {
	if (len(config.Apps) == 0) {
		return errors.New("no apps found, not updating nginx config to avoid deleting existing config")
	}

	template, err := template.New(filepath.Base(config.Nginx_template)).ParseFiles(config.Nginx_template)
	if err != nil {
		return err
	}

	parent := filepath.Dir(config.Nginx_config)
	tmpFile, err := ioutil.TempFile(parent, ".nginx.conf.tmp-")
	defer tmpFile.Close()

	err = template.Execute(tmpFile, config)
	if err != nil {
		return err
	}
	config.LastUpdates.LastConfigRendered = time.Now()

	err = checkConf(tmpFile.Name())
	if err != nil {
		return err
	}

	err = os.Rename(tmpFile.Name(), config.Nginx_config)
	if err != nil {
		return err
	}

	return nil
}

func checkTmpl() error {
	t, err := template.New(filepath.Base(config.Nginx_template)).ParseFiles(config.Nginx_template)
	if err != nil {
		return err
	}
	err = t.Execute(ioutil.Discard, config)
	if err != nil {
		return err
	}
	return nil
}

func checkConf(path string) error {
	cmd := exec.Command(config.Nginx_cmd, "-c", path, "-t")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run() // will wait for command to return
	if err != nil {
		msg := fmt.Sprint(err) + ": " + stderr.String()
		errstd := errors.New(msg)
		return errstd
	}
	return nil
}

func reloadNginx() error {
	cmd := exec.Command(config.Nginx_cmd, "-s", "reload")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run() // will wait for command to return
	if err != nil {
		msg := fmt.Sprint(err) + ": " + stderr.String()
		errstd := errors.New(msg)
		return errstd
	}
	return nil
}

func reload() error {
	jsontasks := MarathonTasks{}
	jsonapps := MarathonApps{}
	err := fetchApps(&jsontasks, &jsonapps)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Error("unable to sync from marathon")
		return err
	}
	syncApps(&jsontasks, &jsonapps)
	config.LastUpdates.LastSync = time.Now()
	err = writeConf()
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Error("unable to generate nginx config")
		return err
	}
	config.LastUpdates.LastConfigValid = time.Now()
	err = reloadNginx()
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Error("unable to reload nginx")
		return err
	}
	config.LastUpdates.LastNginxReload = time.Now()
	return nil
}
