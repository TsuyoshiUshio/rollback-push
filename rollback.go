package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"code.cloudfoundry.org/cli/plugin"
	"github.com/contraband/autopilot/rewind"
)

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stdout, "error:", err)
		os.Exit(1)
	}
}

func main() {
	plugin.Start(&RollbackPlugin{})
}

type RollbackPlugin struct{}

func g1AppName(appName string) string {
	return fmt.Sprintf("%s-g1", appName)
}
func g2AppName(appName string) string {
	return fmt.Sprintf("%s-g2", appName)
}

func getActionsForExistingApp(appRepo *ApplicationRepo, appName, manifestPath, appPath string, g1Exists bool, g2Exists bool) []rewind.Action {
	return []rewind.Action{
		// // versioning
		// {
		// 	Forward: func() error {
		// 		if g2Exists {
		// 			appRepo.DeleteApplication(g2AppName(appName))
		// 		}
		// 		if g1Exists {
		// 			appRepo.RenameApplication(g1AppName(appName), g2AppName(appName))
		// 		}
		// 		return
		// 	},
		// },
		// rename
		{
			Forward: func() error {
				// versioning
				if g2Exists {
					appRepo.DeleteApplication(g2AppName(appName))
				}
				if g1Exists {
					appRepo.RenameApplication(g1AppName(appName), g2AppName(appName))
				}
				return appRepo.RenameApplication(appName, g1AppName(appName))
			},
		},
		// push
		{
			Forward: func() error {
				return appRepo.PushApplication(appName, manifestPath, appPath)
			},
			ReversePrevious: func() error {
				// If the app cannot start we'll have a lingering application
				// We delete this application so that the rename can succeed
				appRepo.DeleteApplication(appName)

				return appRepo.RenameApplication(g1AppName(appName), appName)
			},
		},
		// unmap-route and stop
		{
			Forward: func() error {
				appRepo.UnMapRouteApplication(g1AppName(appName), appName)
				return appRepo.StopApplication(g1AppName(appName))
			},
		},
	}
}

func getActionsForNewApp(appRepo *ApplicationRepo, appName, manifestPath, appPath string) []rewind.Action {
	return []rewind.Action{
		// push
		{
			Forward: func() error {
				return appRepo.PushApplication(appName, manifestPath, appPath)
			},
		},
	}
}

func (plugin RollbackPlugin) Run(cliConnection plugin.CliConnection, args []string) {
	appRepo := NewApplicationRepo(cliConnection)
	appName, manifestPath, appPath, err := ParseArgs(args)
	fatalIf(err)

	appExists, err := appRepo.DoesAppExist(appName)
	fatalIf(err)

	g1Exists, err := appRepo.DoesAppExist(appName + "-g1")
	fatalIf(err)

	g2Exists, err := appRepo.DoesAppExist(appName + "-g2")

	var actionList []rewind.Action

	if appExists {
		actionList = getActionsForExistingApp(appRepo, appName, manifestPath, appPath, g1Exists, g2Exists)
	} else {
		actionList = getActionsForNewApp(appRepo, appName, manifestPath, appPath)
	}

	actions := rewind.Actions{
		Actions:              actionList,
		RewindFailureMessage: "Oh no. Something's gone wrong. I've tried to roll back but you should check to see if everything is OK.",
	}

	err = actions.Execute()
	fatalIf(err)

	fmt.Println()
	fmt.Println("A new version of your application has successfully been pushed!")
	fmt.Println()

	_ = appRepo.ListApplications()
}

func (RollbackPlugin) GetMetadata() plugin.PluginMetadata {
	return plugin.PluginMetadata{
		Name: "rollback",
		Version: plugin.VersionType{
			Major: 0,
			Minor: 0,
			Build: 2,
		},
		Commands: []plugin.Command{
			{
				Name:     "blue-green-push",
				HelpText: "Perform a zero-downtime push with versioning feature of an application over the top of an old one",
				UsageDetails: plugin.Usage{
					Usage: "$ cf blue-green-push application-to-replace \\ \n \t-f path/to/new_manifest.yml \\ \n \t-p path/to/new/path",
				},
			},
		},
	}
}

func ParseArgs(args []string) (string, string, string, error) {
	flags := flag.NewFlagSet("blue-green-push", flag.ContinueOnError)
	manifestPath := flags.String("f", "", "path to an application manifest")
	appPath := flags.String("p", "", "path to application files")

	err := flags.Parse(args[2:])
	if err != nil {
		return "", "", "", err
	}

	appName := args[1]

	if *manifestPath == "" {
		return "", "", "", ErrNoManifest
	}

	return appName, *manifestPath, *appPath, nil
}

var ErrNoManifest = errors.New("a manifest is required to push this application")

type ApplicationRepo struct {
	conn plugin.CliConnection
}

func NewApplicationRepo(conn plugin.CliConnection) *ApplicationRepo {
	return &ApplicationRepo{
		conn: conn,
	}
}

func (repo *ApplicationRepo) UnMapRouteApplication(appName string, hostName string) error {
	result, err := repo.conn.GetApp(appName)
	// fmt.Println(result)
	if err != nil {
		return err
	}
	_, err = repo.conn.CliCommand("unmap-route", appName, result.Routes[0].Domain.Name, "-n", hostName)
	return err
}

func (repo *ApplicationRepo) StopApplication(appName string) error {
	_, err := repo.conn.CliCommand("stop", appName)
	return err
}

func (repo *ApplicationRepo) RenameApplication(oldName, newName string) error {
	_, err := repo.conn.CliCommand("rename", oldName, newName)
	return err
}

func (repo *ApplicationRepo) PushApplication(appName, manifestPath, appPath string) error {
	args := []string{"push", appName, "-f", manifestPath}

	if appPath != "" {
		args = append(args, "-p", appPath)
	}

	_, err := repo.conn.CliCommand(args...)
	return err
}

func (repo *ApplicationRepo) DeleteApplication(appName string) error {
	_, err := repo.conn.CliCommand("delete", appName, "-f")
	return err
}

func (repo *ApplicationRepo) ListApplications() error {
	_, err := repo.conn.CliCommand("apps")
	return err
}

func (repo *ApplicationRepo) DoesAppExist(appName string) (bool, error) {
	space, err := repo.conn.GetCurrentSpace()
	if err != nil {
		return false, err
	}

	path := fmt.Sprintf(`v2/apps?q=name:%s&q=space_guid:%s`, url.QueryEscape(appName), space.Guid)
	result, err := repo.conn.CliCommandWithoutTerminalOutput("curl", path)

	if err != nil {
		return false, err
	}

	jsonResp := strings.Join(result, "")

	output := make(map[string]interface{})
	err = json.Unmarshal([]byte(jsonResp), &output)

	if err != nil {
		return false, err
	}

	totalResults, ok := output["total_results"]

	if !ok {
		return false, errors.New("Missing total_results from api response")
	}

	count, ok := totalResults.(float64)

	if !ok {
		return false, fmt.Errorf("total_results didn't have a number %v", totalResults)
	}

	return count == 1, nil
}
