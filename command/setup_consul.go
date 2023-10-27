// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package command

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

// Ensure SetupConsulCommand satisfies the cli.Command interface.
var _ cli.Command = &SetupConsulCommand{}

//go:embed asset/consul-wi-default-auth-method-config.json
var authConfigBody []byte

//go:embed asset/consul-wi-default-policy.hcl
var policyBody []byte

const (
	authMethodServices = structs.ConsulServicesDefaultAuthMethodName
	authMethodTasks    = structs.ConsulTasksDefaultAuthMethodName
	roleTasks          = "role-nomad-tasks"
	policyName         = "policy-nomad-tasks"
	consulNamespace    = "nomad-prod"
	aud                = "consul.io"
)

type SetupConsulCommand struct {
	Meta

	// client is the Consul API client shared by all functions in the command to
	// reuse the same connection.
	client *api.Client

	jwksURL string

	consulEnt bool
	autoYes   bool
}

// Help satisfies the cli.Command Help function.
func (s *SetupConsulCommand) Help() string {
	helpText := `
Usage: nomad setup consul [options]

  This command sets up Consul for allowing Nomad workloads to authenticate
  themselves using Workload Identity.

  This command requires acl:write permissions for Consul and will respect
  CONSUL_HTTP_TOKEN as well as CONSUL_HTTP_ADDR and other Consul-related
  environment variables as documented in
  https://developer.hashicorp.com/nomad/docs/runtime/environment#summary. 

Setup Consul options:

  -jwks-url
    URL of the JWKS server Consul will contact in order to verify JWT
    signatures. Defaults to http://localhost:4646/.well-known/jwks.json. 

  -y
    Automatically answers "yes" to all the questions, making the setup
    non-interactive. Defaults to "false".

`
	return strings.TrimSpace(helpText)
}

func (s *SetupConsulCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(s.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-jwks-url": complete.PredictAnything,
			"-y":        complete.PredictSet("true", "false"),
		})
}

func (s *SetupConsulCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

// Synopsis satisfies the cli.Command Synopsis function.
func (s *SetupConsulCommand) Synopsis() string { return "Setup a Consul cluster for Nomad integration" }

// Name returns the name of this command.
func (s *SetupConsulCommand) Name() string { return "setup consul" }

// Run satisfies the cli.Command Run function.
func (s *SetupConsulCommand) Run(args []string) int {

	flags := s.Meta.FlagSet(s.Name(), FlagSetClient)
	flags.Usage = func() { s.Ui.Output(s.Help()) }
	flags.BoolVar(&s.autoYes, "y", false, "")
	flags.StringVar(&s.jwksURL, "jwks-url", "http://localhost:4646/.well-known/jwks.json", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	// Check that we got no arguments.
	if len(flags.Args()) != 0 {
		s.Ui.Error("This command takes no arguments")
		s.Ui.Error(commandErrorText(s))
		return 1
	}

	var err error

	s.Ui.Output(`
This command will walk you through configuring all the components required for 
Nomad workloads to authenticate themselves against Consul ACL using their 
respective workload identities. 

First we need to connect to Consul. 
`)

	cfg := api.DefaultConfig()
	if !s.autoYes {
		if !s.askQuestion(fmt.Sprintf("Is %s the correct address of your Consul cluster? [Y/n]", cfg.Address)) {
			s.Ui.Warn(`
Please set the CONSUL_HTTP_ADDR variable to your Consul cluster address and re-run the command.`)
			return 0
		}
	}

	// Get the Consul client.
	s.client, err = api.NewClient(cfg)
	if err != nil {
		s.Ui.Error(fmt.Sprintf("Error initializing Consul client: %s", err))
		return 1
	}

	// check if we're connecting to Consul ent
	if _, err := s.client.Operator().LicenseGet(nil); err == nil {
		s.consulEnt = true
	}

	authMethodMsg := `
We need to create two JWT auth methods: one for Nomad services, and one for
Nomad tasks. The method for services will be called %v and the
method for tasks %v, and they will both be of jwt type. They
will share the following config:`
	s.Ui.Output(fmt.Sprintf(authMethodMsg, authMethodServices, authMethodTasks))

	authMethodConf, err := s.renderAuthMethodConf()
	if err != nil {
		s.Ui.Error(err.Error())
		return 1
	}

	jsConf, _ := json.MarshalIndent(authMethodConf, "", "    ")
	s.Ui.Output(string(jsConf))

	if s.consulEnt {
		namespaceMsg := `
Since you're running Consul Enterprise, we will additionally create
a namespace %s and bind the auth methods to that namespace.
`
		s.Ui.Output(fmt.Sprintf(namespaceMsg, consulNamespace))
	}

	var createAuthMethods bool
	if !s.autoYes {
		createAuthMethods = s.askQuestion("Create these auth methods in your Consul cluster? [Y/n]")
	} else {
		createAuthMethods = true
	}

	if s.consulEnt {
		err = s.createNamespace()
		if err != nil {
			s.Ui.Error(err.Error())
			return 1
		}
	}

	if createAuthMethods {
		err = s.createAuthMethod(authMethodServices, authMethodConf)
		if err != nil {
			s.Ui.Error(err.Error())
			return 1
		}
		err = s.createAuthMethod(authMethodTasks, authMethodConf)
		if err != nil {
			s.Ui.Error(err.Error())
			return 1
		}
	}

	servicesBindingRule := &api.ACLBindingRule{
		Description: "binding rule for Nomad services w/ (WI)",
		AuthMethod:  authMethodServices,
		BindType:    "service",
		BindName:    "${value.nomad_namespace}-${value.nomad_service}",
	}

	tasksBindingRule := &api.ACLBindingRule{
		Description: "binding rule for Nomad tasks w/ (WI)",
		AuthMethod:  authMethodTasks,
		BindType:    "role",
		BindName:    "nomad-${value.nomad_namespace}-templates",
	}

	s.Ui.Output(`
In order to map claims between Nomad's JWTs and Consul ACL, we need to create
the following binding rules:
`)
	jsServicesBindingRule, _ := json.MarshalIndent(servicesBindingRule, "", "    ")
	jsTasksBindingRule, _ := json.MarshalIndent(tasksBindingRule, "", "    ")
	s.Ui.Output(string(jsServicesBindingRule))
	s.Ui.Output(string(jsTasksBindingRule))

	var createBindingRules bool
	if !s.autoYes {
		createBindingRules = s.askQuestion(
			"Create these binding rules in your Consul cluster? [Y/n]",
		)
	} else {
		createBindingRules = true
	}

	if createBindingRules {
		err = s.createBindingRules(servicesBindingRule)
		if err != nil {
			s.Ui.Error(err.Error())
			return 1
		}
		err = s.createBindingRules(tasksBindingRule)
		if err != nil {
			s.Ui.Error(err.Error())
			return 1
		}
	}

	s.Ui.Output(`
Nomad tasks require a Consul ACL policy and Role. Below is the body of the policy
we need to create:
`)
	s.Ui.Output(string(policyBody))

	var createPolicy bool
	if !s.autoYes {
		createPolicy = s.askQuestion(
			"Should we create the above policy in your Consul cluster? [Y/n]",
		)
	} else {
		createPolicy = true
	}

	if createPolicy {
		err = s.createPolicy()
		if err != nil {
			s.Ui.Error(err.Error())
			return 1
		}
	}

	s.Ui.Output(`
Finally, we need to create a role role-nomad-tasks associated with the policy
above.`)

	var createRole bool
	if !s.autoYes {
		createRole = s.askQuestion(
			"Should we create the above role in your Consul cluster? [Y/n]",
		)
	} else {
		createRole = true
	}

	if createRole {
		err = s.createRoleForTasks()
		if err != nil {
			s.Ui.Error(err.Error())
			return 1
		}
	}

	s.Ui.Output(`
Congratulations, your Consul cluster is now setup and ready to accept Nomad
workloads with Workload Identity!

You need to adjust your Nomad client configuration in the following way:

consul {
  enabled = true
  address = "<Consul address>"

  # Nomad agents still need a Consul token in order to register themselves
  # for automated clustering. It is recommended to set the token using the
  # CONSUL_HTTP_TOKEN environment variable instead of writing it in the
  # configuration file.
}

and the configuration of your Nomad servers as follows:

consul {
  enabled = true
  address = "<Consul address>"

  # Nomad agents still need a Consul token in order to register themselves
  # for automated clustering. It is recommended to set the token using the
  # CONSUL_HTTP_TOKEN environment variable instead of writing it in the
  # configuration file.

  service_identity {
    aud = ["consul.io"]
    ttl = "1h"
  }

  task_identity {
    aud = ["consul.io"]
    ttl = "1h"
  }
}

`)

	return 0
}

func (s *SetupConsulCommand) renderAuthMethodConf() (map[string]any, error) {
	authConfig := map[string]any{}
	err := json.Unmarshal(authConfigBody, &authConfig)
	if err != nil {
		return authConfig, fmt.Errorf("default auth config text could not be deserialized: %v", err)
	}

	authConfig["JWKSURL"] = s.jwksURL
	authConfig["BoundAudiences"] = []string{aud}
	authConfig["JWTSupportedAlgs"] = []string{"RS256"}

	return authConfig, nil
}

func (s *SetupConsulCommand) createAuthMethod(authMethodName string, authMethodConf map[string]any) error {
	method := &api.ACLAuthMethod{
		Name:          authMethodName,
		Type:          "jwt",
		DisplayName:   authMethodName,
		Description:   "login method for Nomad workload identities (WI)",
		TokenLocality: "local",
		Config:        authMethodConf,
	}

	if s.consulEnt {
		method.NamespaceRules = []*api.ACLAuthMethodNamespaceRule{{
			Selector:      "",
			BindNamespace: "${value.nomad_namespace}",
		}}
	}

	existingMethods, _, _ := s.client.ACL().AuthMethodList(nil)
	if len(existingMethods) > 0 {
		if slices.ContainsFunc(
			existingMethods,
			func(m *api.ACLAuthMethodListEntry) bool { return m.Name == method.Name }) {
			s.Ui.Warn(fmt.Sprintf("[ ] auth method with name %s already exists", method.Name))
			return nil
		}
	}

	_, _, err := s.client.ACL().AuthMethodCreate(method, nil)
	if err != nil {
		if strings.Contains(err.Error(), "error checking JWKSURL") {
			s.Ui.Error("error: Nomad JWKS endpoint unreachable, is your Nomad server running and is the JWKS url set correctly?")
			os.Exit(1)
		}
		return fmt.Errorf("[✘] could not create Consul auth method: %w", err)
	}

	s.Ui.Info(fmt.Sprintf("[✔] Created auth method %s", authMethodName))
	return nil
}

func (s *SetupConsulCommand) createNamespace() error {
	nsClient := s.client.Namespaces()

	namespace := &api.Namespace{Name: consulNamespace}

	// check if namespace already exists
	existingNamespaces, _, _ := nsClient.List(nil)
	if len(existingNamespaces) > 0 {
		if slices.ContainsFunc(
			existingNamespaces,
			func(n *api.Namespace) bool { return n.Name == consulNamespace }) {
			s.Ui.Warn(fmt.Sprintf("[ ] namespace %s already exists", consulNamespace))
			return nil
		}
	}

	_, _, err := nsClient.Create(namespace, nil)
	if err != nil {
		return fmt.Errorf("[✘] could not write namespace %q: %w", consulNamespace, err)
	}
	s.Ui.Info(fmt.Sprintf("[✔] Created namespace %s", consulNamespace))
	return nil
}

func (s *SetupConsulCommand) createBindingRules(rule *api.ACLBindingRule) error {
	existingRules, _, _ := s.client.ACL().BindingRuleList("", nil)
	if len(existingRules) > 0 {
		if slices.ContainsFunc(
			existingRules,
			func(r *api.ACLBindingRule) bool { return r.BindName == rule.BindName }) {
			s.Ui.Warn(fmt.Sprintf("[ ] binding rule with bind name %s already exists", rule.BindName))
			return nil
		}
	}

	_, _, err := s.client.ACL().BindingRuleCreate(rule, nil)
	if err != nil {
		return fmt.Errorf("[✘] could not create Consul binding rule: %w", err)
	}

	s.Ui.Info(fmt.Sprintf("[✔] Created binding rule for auth method %s", rule.AuthMethod))
	return nil
}

func (s *SetupConsulCommand) createRoleForTasks() error {
	_, _, err := s.client.ACL().RoleCreate(&api.ACLRole{
		Name:        roleTasks,
		Description: "role for Nomad templates w/ workload identities (WI)",
		Policies:    []*api.ACLLink{{Name: policyName}},
	}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			s.Ui.Warn(fmt.Sprintf("[ ] role %s already exists", roleTasks))
			return nil
		}
		return fmt.Errorf("[✘] could not create Consul role: %w", err)
	}

	s.Ui.Info(fmt.Sprintf("[✔] Created role %s\n", roleTasks))
	return nil
}

func (s *SetupConsulCommand) createPolicy() error {
	_, _, err := s.client.ACL().PolicyCreate(&api.ACLPolicy{
		Name:  policyName,
		Rules: string(policyBody),
	}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			s.Ui.Warn(fmt.Sprintf("[ ] policy %s already exists", policyName))
			return nil
		}
		return fmt.Errorf("[✘] could not create Consul policy: %w", err)
	}

	s.Ui.Info(fmt.Sprintf("[✔] Created policy %s", policyName))

	return nil
}

// askQuestion asks question to user until they provide a valid response.
func (s *SetupConsulCommand) askQuestion(question string) bool {
	for {
		answer, err := s.Ui.Ask(s.Colorize().Color(fmt.Sprintf("[?] %s", question)))
		if err != nil {
			if err.Error() != "interrupted" {
				s.Ui.Output(err.Error())
				os.Exit(1)
			}
			os.Exit(0)
		}

		switch strings.TrimSpace(strings.ToLower(answer)) {
		case "", "y", "yes":
			return true
		case "n", "no":
			return false
		default:
			s.Ui.Output(fmt.Sprintf(`%q is not a valid response, please answer "yes" or "no".`, answer))
			continue
		}
	}
}
