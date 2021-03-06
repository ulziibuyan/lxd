package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxc/config"
	"github.com/lxc/lxd/lxc/utils"
	"github.com/lxc/lxd/shared/api"
	cli "github.com/lxc/lxd/shared/cmd"
	"github.com/lxc/lxd/shared/i18n"
	"github.com/lxc/lxd/shared/termios"
)

type cmdInit struct {
	global *cmdGlobal

	flagConfig     []string
	flagEphemeral  bool
	flagNetwork    string
	flagProfile    []string
	flagStorage    string
	flagTarget     string
	flagType       string
	flagNoProfiles bool
	flagEmpty      bool
}

func (c *cmdInit) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = i18n.G("init [[<remote>:]<image>] [<remote>:][<name>] [< config")
	cmd.Short = i18n.G("Create containers from images")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(`Create containers from images`))
	cmd.Example = cli.FormatSection("", i18n.G(`lxc init ubuntu:16.04 u1

lxc init ubuntu:16.04 u1 < config.yaml
    Create the container with configuration from config.yaml`))
	cmd.Hidden = true

	cmd.RunE = c.Run
	cmd.Flags().StringArrayVarP(&c.flagConfig, "config", "c", nil, i18n.G("Config key/value to apply to the new container")+"``")
	cmd.Flags().StringArrayVarP(&c.flagProfile, "profile", "p", nil, i18n.G("Profile to apply to the new container")+"``")
	cmd.Flags().BoolVarP(&c.flagEphemeral, "ephemeral", "e", false, i18n.G("Ephemeral container"))
	cmd.Flags().StringVarP(&c.flagNetwork, "network", "n", "", i18n.G("Network name")+"``")
	cmd.Flags().StringVarP(&c.flagStorage, "storage", "s", "", i18n.G("Storage pool name")+"``")
	cmd.Flags().StringVarP(&c.flagType, "type", "t", "", i18n.G("Instance type")+"``")
	cmd.Flags().StringVar(&c.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.Flags().BoolVar(&c.flagNoProfiles, "no-profiles", false, i18n.G("Create the container with no profiles applied"))
	cmd.Flags().BoolVar(&c.flagEmpty, "empty", false, i18n.G("Create an empty container"))

	return cmd
}

func (c *cmdInit) Run(cmd *cobra.Command, args []string) error {
	// Sanity checks
	exit, err := c.global.CheckArgs(cmd, args, 0, 2)
	if exit {
		return err
	}

	if len(args) == 0 && !c.flagEmpty {
		cmd.Usage()
		return nil
	}

	_, _, err = c.create(c.global.conf, args)
	return err
}

func (c *cmdInit) create(conf *config.Config, args []string) (lxd.InstanceServer, string, error) {
	var name string
	var image string
	var remote string
	var iremote string
	var err error
	var stdinData api.InstancePut
	var devicesMap map[string]map[string]string
	var configMap map[string]string

	// If stdin isn't a terminal, read text from it
	if !termios.IsTerminal(getStdinFd()) {
		contents, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			return nil, "", err
		}

		err = yaml.Unmarshal(contents, &stdinData)
		if err != nil {
			return nil, "", err
		}
	}

	if len(args) > 0 {
		iremote, image, err = conf.ParseRemote(args[0])
		if err != nil {
			return nil, "", err
		}

		if len(args) == 1 {
			remote, name, err = conf.ParseRemote("")
			if err != nil {
				return nil, "", err
			}
		} else if len(args) == 2 {
			remote, name, err = conf.ParseRemote(args[1])
			if err != nil {
				return nil, "", err
			}
		}
	}

	if c.flagEmpty {
		if len(args) > 1 {
			return nil, "", fmt.Errorf(i18n.G("--empty cannot be combined with an image name"))
		}

		if len(args) == 0 {
			remote, name, err = conf.ParseRemote("")
			if err != nil {
				return nil, "", err
			}
		} else if len(args) == 1 {
			// Switch image / container names
			name = image
			remote = iremote
			image = ""
			iremote = ""
		}
	}

	d, err := conf.GetInstanceServer(remote)
	if err != nil {
		return nil, "", err
	}

	if c.flagTarget != "" {
		d = d.UseTarget(c.flagTarget)
	}

	profiles := []string{}
	for _, p := range c.flagProfile {
		profiles = append(profiles, p)
	}

	if !c.global.flagQuiet {
		if name == "" {
			fmt.Printf(i18n.G("Creating the container") + "\n")
		} else {
			fmt.Printf(i18n.G("Creating %s")+"\n", name)
		}
	}

	if len(stdinData.Devices) > 0 {
		devicesMap = stdinData.Devices
	} else {
		devicesMap = map[string]map[string]string{}
	}

	if c.flagNetwork != "" {
		network, _, err := d.GetNetwork(c.flagNetwork)
		if err != nil {
			return nil, "", err
		}

		if network.Type == "bridge" {
			devicesMap[c.flagNetwork] = map[string]string{"type": "nic", "nictype": "bridged", "parent": c.flagNetwork}
		} else {
			devicesMap[c.flagNetwork] = map[string]string{"type": "nic", "nictype": "macvlan", "parent": c.flagNetwork}
		}
	}

	if len(stdinData.Config) > 0 {
		configMap = stdinData.Config
	} else {
		configMap = map[string]string{}
	}
	for _, entry := range c.flagConfig {
		if !strings.Contains(entry, "=") {
			return nil, "", fmt.Errorf(i18n.G("Bad key=value pair: %s"), entry)
		}

		fields := strings.SplitN(entry, "=", 2)
		configMap[fields[0]] = fields[1]
	}

	// Check if the specified storage pool exists.
	if c.flagStorage != "" {
		_, _, err := d.GetStoragePool(c.flagStorage)
		if err != nil {
			return nil, "", err
		}

		devicesMap["root"] = map[string]string{
			"type": "disk",
			"path": "/",
			"pool": c.flagStorage,
		}
	}

	// Setup container creation request
	req := api.InstancesPost{
		Name:         name,
		InstanceType: c.flagType,
	}
	req.Config = configMap
	req.Devices = devicesMap

	if !c.flagNoProfiles && len(profiles) == 0 {
		if len(stdinData.Profiles) > 0 {
			req.Profiles = stdinData.Profiles
		} else {
			req.Profiles = nil
		}
	} else {
		req.Profiles = profiles
	}
	req.Ephemeral = c.flagEphemeral

	var opInfo api.Operation
	if !c.flagEmpty {
		// Get the image server and image info
		iremote, image = c.guessImage(conf, d, remote, iremote, image)
		var imgRemote lxd.ImageServer
		var imgInfo *api.Image

		// Connect to the image server
		if iremote == remote {
			imgRemote = d
		} else {
			imgRemote, err = conf.GetImageServer(iremote)
			if err != nil {
				return nil, "", err
			}
		}

		// Deal with the default image
		if image == "" {
			image = "default"
		}

		// Optimisation for simplestreams
		if conf.Remotes[iremote].Protocol == "simplestreams" {
			imgInfo = &api.Image{}
			imgInfo.Fingerprint = image
			imgInfo.Public = true
			req.Source.Alias = image
		} else {
			// Attempt to resolve an image alias
			alias, _, err := imgRemote.GetImageAlias(image)
			if err == nil {
				req.Source.Alias = image
				image = alias.Target
			}

			// Get the image info
			imgInfo, _, err = imgRemote.GetImage(image)
			if err != nil {
				return nil, "", err
			}
		}

		// Create the container
		op, err := d.CreateInstanceFromImage(imgRemote, *imgInfo, req)
		if err != nil {
			return nil, "", err
		}

		// Watch the background operation
		progress := utils.ProgressRenderer{
			Format: i18n.G("Retrieving image: %s"),
			Quiet:  c.global.flagQuiet,
		}

		_, err = op.AddHandler(progress.UpdateOp)
		if err != nil {
			progress.Done("")
			return nil, "", err
		}

		err = utils.CancelableWait(op, &progress)
		if err != nil {
			progress.Done("")
			return nil, "", err
		}
		progress.Done("")

		// Extract the container name
		info, err := op.GetTarget()
		if err != nil {
			return nil, "", err
		}

		opInfo = *info
	} else {
		req.Source.Type = "none"

		op, err := d.CreateInstance(req)
		if err != nil {
			return nil, "", err
		}

		opInfo = op.Get()
	}

	containers, ok := opInfo.Resources["containers"]
	if !ok || len(containers) == 0 {
		return nil, "", fmt.Errorf(i18n.G("Didn't get any affected image, container or snapshot from server"))
	}

	if len(containers) == 1 && name == "" {
		fields := strings.Split(containers[0], "/")
		name = fields[len(fields)-1]
		fmt.Printf(i18n.G("Container name is: %s")+"\n", name)
	}

	// Validate the network setup
	c.checkNetwork(d, name)

	return d, name, nil
}

func (c *cmdInit) guessImage(conf *config.Config, d lxd.InstanceServer, remote string, iremote string, image string) (string, string) {
	if remote != iremote {
		return iremote, image
	}

	fields := strings.SplitN(image, "/", 2)
	_, ok := conf.Remotes[fields[0]]
	if !ok {
		return iremote, image
	}

	_, _, err := d.GetImageAlias(image)
	if err == nil {
		return iremote, image
	}

	_, _, err = d.GetImage(image)
	if err == nil {
		return iremote, image
	}

	if len(fields) == 1 {
		fmt.Fprintf(os.Stderr, i18n.G("The local image '%s' couldn't be found, trying '%s:' instead.")+"\n", image, fields[0])
		return fields[0], "default"
	}

	fmt.Fprintf(os.Stderr, i18n.G("The local image '%s' couldn't be found, trying '%s:%s' instead.")+"\n", image, fields[0], fields[1])
	return fields[0], fields[1]
}

func (c *cmdInit) checkNetwork(d lxd.InstanceServer, name string) {
	ct, _, err := d.GetInstance(name)
	if err != nil {
		return
	}

	for _, d := range ct.ExpandedDevices {
		if d["type"] == "nic" {
			return
		}
	}

	fmt.Fprintf(os.Stderr, "\n"+i18n.G("The container you are starting doesn't have any network attached to it.")+"\n")
	fmt.Fprintf(os.Stderr, "  "+i18n.G("To create a new network, use: lxc network create")+"\n")
	fmt.Fprintf(os.Stderr, "  "+i18n.G("To attach a network to a container, use: lxc network attach")+"\n\n")
}
