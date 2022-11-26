package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/fatih/structs"
	"github.com/gorilla/mux"
	"github.com/malice-plugins/pkgs/database"
	"github.com/malice-plugins/pkgs/database/elasticsearch"
	"github.com/malice-plugins/pkgs/utils"
	"github.com/parnurzeal/gorequest"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

const (
	name     = "drweb"
	category = "av"
)

var (
	// Version stores the plugin's version
	Version string
	// BuildTime stores the plugin's build time
	BuildTime string
	// LicenseKey stores the valid Dr.Web license key
	LicenseKey string
	path       string
	hash       string
	// es is the elasticsearch database object
	es elasticsearch.Database
)

type pluginResults struct {
	ID   string      `json:"id" structs:"id,omitempty"`
	Data ResultsData `json:"drweb" structs:"drweb"`
}

// DrWEB json object
type DrWEB struct {
	Results ResultsData `json:"drweb"`
}

// ResultsData json object
type ResultsData struct {
	Infected bool   `json:"infected" structs:"infected"`
	Result   string `json:"result" structs:"result"`
	Engine   string `json:"engine" structs:"engine"`
	Database string `json:"database" structs:"database"`
	Updated  string `json:"updated" structs:"updated"`
	MarkDown string `json:"markdown,omitempty" structs:"markdown,omitempty"`
	Error    string `json:"error,omitempty" structs:"error,omitempty"`
}

func assert(err error) {
	if err != nil {
		// skip exit code 13 (which means a virus was found)
		if err.Error() != "exit status 13" {
			log.WithFields(log.Fields{
				"plugin":   name,
				"category": category,
				"path":     path,
			}).Fatal(err)
		}
	}
}

// AvScan performs antivirus scan
func AvScan(timeout int) DrWEB {

	var output string
	var sErr error

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	expired, err := didLicenseExpire(ctx)
	assert(err)
	if expired {
		err = updateLicense(ctx)
		assert(err)
	}

	// drweb needs to have the daemon started first
	configd := exec.CommandContext(ctx, "/opt/drweb.com/bin/drweb-configd", "-d")
	_, err = configd.Output()
	assert(err)
	defer configd.Process.Kill()

	time.Sleep(1 * time.Second)

	log.Debug("running drweb-ctl scan")
	output, sErr = utils.RunCommand(ctx, "/opt/drweb.com/bin/drweb-ctl", "scan", path)
	if sErr != nil {
		// If fails try a second time
		time.Sleep(10 * time.Second)
		log.Debug("re-running drweb-ctl scan")
		output, sErr = utils.RunCommand(ctx, "/opt/drweb.com/bin/drweb-ctl", "scan", path)
	}

	baseinfo, err := utils.RunCommand(ctx, "/opt/drweb.com/bin/drweb-ctl", "baseinfo")
	assert(err)

	results, err := ParseDrWEBOutput(output, baseinfo, sErr)

	return DrWEB{Results: results}
}

// ParseDrWEBOutput convert drweb output into ResultsData struct
func ParseDrWEBOutput(drwebOut, baseInfo string, drwebErr error) (ResultsData, error) {

	log.WithFields(log.Fields{
		"plugin":   name,
		"category": category,
		"path":     path,
	}).Debug("Dr.WEB Output: ", drwebOut)

	if drwebErr != nil {
		if drwebErr.Error() == "exit status 119" {
			return ResultsData{Error: "ScanEngine is not available"}, drwebErr
		}
		return ResultsData{Error: drwebErr.Error()}, drwebErr
	}

	drweb := ResultsData{
		Infected: false,
		Engine:   getDrWebVersion(),
		Updated:  getUpdatedDate(),
	}

	for _, line := range strings.Split(drwebOut, "\n") {
		if len(line) != 0 {
			if strings.Contains(line, "- Ok") {
				break

			} else {

				drweb.Infected = true
				drweb.Result = strings.TrimSpace(strings.TrimPrefix(line, path+" - "))

			}

			//if strings.Contains(line, "infected with") {
			//	drweb.Infected = true
			//	drweb.Result = strings.TrimSpace(strings.TrimPrefix(line, path+" - infected with"))
			//}
			//
			//if strings.Contains(line, "threats found:") {
			//	threat := strings.TrimPrefix(strings.TrimSpace(line), "threats found:")
			//	if len(threat) > 0 {
			//		drweb.Infected = true
			//		drweb.Result = strings.TrimSpace(strings.TrimPrefix(line, path+" - "))
			//	}
			//}

		}
	}

	log.WithFields(log.Fields{
		"plugin":   name,
		"category": category,
		"path":     path,
	}).Debug("Dr.WEB Base Info: ", baseInfo)

	for _, line := range strings.Split(baseInfo, "\n") {
		if len(line) != 0 {
			if strings.Contains(line, "Core engine:") {
				drweb.Engine = strings.TrimSpace(strings.TrimPrefix(line, "Core engine:"))
			}
			if strings.Contains(line, "Virus base records:") {
				drweb.Database = strings.TrimSpace(strings.TrimPrefix(line, "Virus base records:"))
			}
		}
	}

	return drweb, nil
}

func getDrWebVersion() string {

	versionOut, err := utils.RunCommand(nil, "/opt/drweb.com/bin/drweb-ctl", "--version")
	assert(err)

	log.Debug("DrWEB Version: ", versionOut)
	return strings.TrimSpace(strings.TrimPrefix(versionOut, "drweb-ctl "))
}

func parseUpdatedDate(date string) string {
	layout := "Mon, 02 Jan 2006 15:04:05 +0000"
	t, _ := time.Parse(layout, date)
	return fmt.Sprintf("%d%02d%02d", t.Year(), t.Month(), t.Day())
}

func getUpdatedDate() string {
	if _, err := os.Stat("/opt/malice/UPDATED"); os.IsNotExist(err) {
		return BuildTime
	}
	updated, err := ioutil.ReadFile("/opt/malice/UPDATED")
	assert(err)
	return string(updated)
}

func updateAV(ctx context.Context) error {
	// drweb needs to have the daemon started first
	configd := exec.Command("/opt/drweb.com/bin/drweb-configd", "-d")
	_, err := configd.Output()
	assert(err)
	defer configd.Process.Kill()

	fmt.Println("Updating Dr.WEB...")
	fmt.Println(utils.RunCommand(ctx, "/opt/drweb.com/bin/drweb-ctl", "update"))
	// Update UPDATED file
	t := time.Now().Format("20060102")
	err = ioutil.WriteFile("/opt/malice/UPDATED", []byte(t), 0644)
	return err
}

func updateLicense(ctx context.Context) error {
	// drweb needs to have the daemon started first
	configd := exec.CommandContext(ctx, "/opt/drweb.com/bin/drweb-configd", "-d")
	_, err := configd.Output()
	if err != nil {
		return err
	}
	defer configd.Process.Kill()
	time.Sleep(1 * time.Second)

	// check for exec context timeout
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("command updateLicense() timed out")
	}

	log.Debug("updating Dr.WEB license")
	if len(LicenseKey) > 0 {
		log.Debugln(utils.RunCommand(ctx, "/opt/drweb.com/bin/drweb-ctl", "license", "--GetRegistered", LicenseKey))
	} else {
		log.Debugln(utils.RunCommand(ctx, "/opt/drweb.com/bin/drweb-ctl", "license", "--GetDemo"))
	}

	return nil
}

func didLicenseExpire(ctx context.Context) (bool, error) {
	// drweb needs to have the daemon started first
	configd := exec.CommandContext(ctx, "/opt/drweb.com/bin/drweb-configd", "-d")
	_, err := configd.Output()
	if err != nil {
		return false, err
	}
	defer configd.Process.Kill()
	time.Sleep(1 * time.Second)

	log.Debug("checking Dr.WEB license")
	license := exec.CommandContext(ctx, "/opt/drweb.com/bin/drweb-ctl", "license")
	lOut, err := license.Output()
	if err != nil {
		return false, err
	}

	if strings.Contains(string(lOut), "No license") {
		log.Debug("no licence found or licence has been invalidated")
		return true, nil
	}

	if strings.Contains(string(lOut), "expires") {
		return false, nil
	}

	log.WithFields(log.Fields{"output": string(lOut)}).Debug("licence expired")
	return true, nil
}

func generateMarkDownTable(a DrWEB) string {
	var tplOut bytes.Buffer

	t := template.Must(template.New("drweb").Parse(tpl))

	err := t.Execute(&tplOut, a)
	if err != nil {
		log.Println("executing template:", err)
	}

	return tplOut.String()
}

func printStatus(resp gorequest.Response, body string, errs []error) {
	fmt.Println(body)
}

func webService() {
	router := mux.NewRouter().StrictSlash(true)
	router.HandleFunc("/scan", webAvScan).Methods("POST")
	log.WithFields(log.Fields{
		"plugin":   name,
		"category": category,
	}).Info("web service listening on port :3993")
	log.Fatal(http.ListenAndServe(":3993", router))
}

func webAvScan(w http.ResponseWriter, r *http.Request) {

	r.ParseMultipartForm(32 << 20)
	file, header, err := r.FormFile("malware")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "Please supply a valid file to scan.")
		log.WithFields(log.Fields{
			"plugin":   name,
			"category": category,
		}).Error(err)
	}
	defer file.Close()

	log.WithFields(log.Fields{
		"plugin":   name,
		"category": category,
	}).Debug("Uploaded fileName: ", header.Filename)

	tmpfile, err := ioutil.TempFile("/malware", "web_")
	assert(err)
	defer os.Remove(tmpfile.Name()) // clean up

	data, err := ioutil.ReadAll(file)
	assert(err)

	if _, err = tmpfile.Write(data); err != nil {
		assert(err)
	}
	if err = tmpfile.Close(); err != nil {
		assert(err)
	}

	// Do AV scan
	path = tmpfile.Name()
	drweb := AvScan(60)

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(drweb); err != nil {
		assert(err)
	}
}

func main() {

	cli.AppHelpTemplate = utils.AppHelpTemplate
	app := cli.NewApp()

	app.Name = "drweb"
	app.Author = "blacktop"
	app.Email = "https://github.com/blacktop"
	app.Version = Version + ", BuildTime: " + BuildTime
	app.Compiled, _ = time.Parse("20060102", BuildTime)
	app.Usage = "Malice Dr.WEB AntiVirus Plugin"
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "verbose, V",
			Usage: "verbose output",
		},
		cli.StringFlag{
			Name:        "elasticsearch",
			Value:       "",
			Usage:       "elasticsearch url for Malice to store results",
			EnvVar:      "MALICE_ELASTICSEARCH_URL",
			Destination: &es.URL,
		},
		cli.BoolFlag{
			Name:  "table, t",
			Usage: "output as Markdown table",
		},
		cli.BoolFlag{
			Name:   "callback, c",
			Usage:  "POST results back to Malice webhook",
			EnvVar: "MALICE_ENDPOINT",
		},
		cli.BoolFlag{
			Name:   "proxy, x",
			Usage:  "proxy settings for Malice webhook endpoint",
			EnvVar: "MALICE_PROXY",
		},
		cli.IntFlag{
			Name:   "timeout",
			Value:  120,
			Usage:  "malice plugin timeout (in seconds)",
			EnvVar: "MALICE_TIMEOUT",
		},
	}
	app.Commands = []cli.Command{
		{
			Name:    "update",
			Aliases: []string{"u"},
			Usage:   "Update virus definitions",
			Action: func(c *cli.Context) error {
				return updateAV(nil)
			},
		},
		{
			Name:  "web",
			Usage: "Create a Dr.WEB scan web service",
			Action: func(c *cli.Context) error {
				webService()
				return nil
			},
		},
	}
	app.Action = func(c *cli.Context) error {

		var err error

		if c.Bool("verbose") {
			log.SetLevel(log.DebugLevel)
		}

		if c.Args().Present() {
			path, err = filepath.Abs(c.Args().First())
			assert(err)

			if _, err = os.Stat(path); os.IsNotExist(err) {
				assert(err)
			}

			hash = utils.GetSHA256(path)

			drweb := AvScan(c.Int("timeout"))
			drweb.Results.MarkDown = generateMarkDownTable(drweb)
			// upsert into Database
			if len(c.String("elasticsearch")) > 0 {
				err := es.Init()
				if err != nil {
					return errors.Wrap(err, "failed to initalize elasticsearch")
				}
				err = es.StorePluginResults(database.PluginResults{
					ID:       utils.Getopt("MALICE_SCANID", hash),
					Name:     name,
					Category: category,
					Data:     structs.Map(drweb.Results),
				})
				if err != nil {
					return errors.Wrapf(err, "failed to index malice/%s results", name)
				}
			}

			if c.Bool("table") {
				fmt.Printf(drweb.Results.MarkDown)
			} else {
				drweb.Results.MarkDown = ""
				drwebJSON, err := json.Marshal(drweb)
				assert(err)
				if c.Bool("callback") {
					request := gorequest.New()
					if c.Bool("proxy") {
						request = gorequest.New().Proxy(os.Getenv("MALICE_PROXY"))
					}
					request.Post(os.Getenv("MALICE_ENDPOINT")).
						Set("X-Malice-ID", utils.Getopt("MALICE_SCANID", hash)).
						Send(string(drwebJSON)).
						End(printStatus)

					return nil
				}
				fmt.Println(string(drwebJSON))
			}
		} else {
			log.WithFields(log.Fields{
				"plugin":   name,
				"category": category,
			}).Fatal(fmt.Errorf("Please supply a file to scan with malice/%s", name))
		}
		return nil
	}

	err := app.Run(os.Args)
	assert(err)
}
