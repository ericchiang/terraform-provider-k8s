package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/plugin"
	"github.com/hashicorp/terraform/terraform"
)

type config struct {
	kubeconfig string
	kubectl_path string
}

func main() {
	plugin.Serve(&plugin.ServeOpts{
		ProviderFunc: func() terraform.ResourceProvider {
			return &schema.Provider{
				Schema: map[string]*schema.Schema{
					"kubeconfig": &schema.Schema{
						Type:     schema.TypeString,
						Optional: true,
					},
					"kubectl_path": &schema.Schema{
						Type: 		schema.TypeString,
						Optional: true,
						Default:	"kubectl",
					},
				},
				ResourcesMap: map[string]*schema.Resource{
					"k8s_manifest": resourceManifest(),
				},
				ConfigureFunc: func(d *schema.ResourceData) (interface{}, error) {
					return &config{
						kubeconfig: 	d.Get("kubeconfig").(string),
						kubectl_path: d.Get("kubectl_path").(string),
					}, nil
				},
			}
		},
	})
}

func resourceManifest() *schema.Resource {
	return &schema.Resource{
		Create: resourceManifestCreate,
		Read:   resourceManifestRead,
		Update: resourceManifestUpdate,
		Delete: resourceManifestDelete,

		Schema: map[string]*schema.Schema{
			"content": &schema.Schema{
				Type:      schema.TypeString,
				Required:  true,
				Sensitive: true,
			},
		},
	}
}

func run(cmd *exec.Cmd) error {
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		cmdStr := cmd.Path + " " + strings.Join(cmd.Args, " ")
		if stderr.Len() == 0 {
			return fmt.Errorf("%s: %v", cmdStr, err)
		}
		return fmt.Errorf("%s %v: %s", cmdStr, err, stderr.Bytes())
	}
	return nil
}

func kubectl(m interface{}, args ...string) *exec.Cmd {
	kubeconfig := m.(*config).kubeconfig
	kubectl_path := m.(*config).kubectl_path
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	return exec.Command(kubectl_path, args...)
}

func resourceManifestCreate(d *schema.ResourceData, m interface{}) error {
	cmd := kubectl(m, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(d.Get("content").(string))
	if err := run(cmd); err != nil {
		return err
	}

	stdout := &bytes.Buffer{}
	cmd = kubectl(m, "get", "-f", "-", "-o", "json")
	cmd.Stdin = strings.NewReader(d.Get("content").(string))
	cmd.Stdout = stdout
	if err := run(cmd); err != nil {
		return err
	}

	var data struct {
		Items []struct {
			Metadata struct {
				Selflink string `json:"selflink"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &data); err != nil {
		return fmt.Errorf("decoding response: %v", err)
	}
	if len(data.Items) != 1 {
		return fmt.Errorf("expected to create 1 resource, got %d", len(data.Items))
	}
	selflink := data.Items[0].Metadata.Selflink
	if selflink == "" {
		return fmt.Errorf("could not parse self-link from response %s", stdout.String())
	}
	d.SetId(selflink)
	return nil
}

func resourceManifestUpdate(d *schema.ResourceData, m interface{}) error {
	cmd := kubectl(m, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(d.Get("content").(string))
	return run(cmd)
}

func resourceFromSelflink(s string) (resource, namespace string, ok bool) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	resource = parts[len(parts)-2] + "/" + parts[len(parts)-1]

	for i, part := range parts {
		if part == "namespaces" && len(parts) > i+1 {
			namespace = parts[i+1]
			break
		}
	}
	return resource, namespace, true
}

func resourceManifestDelete(d *schema.ResourceData, m interface{}) error {
	resource, namespace, ok := resourceFromSelflink(d.Id())
	if !ok {
		return fmt.Errorf("invalid resource id: %s", d.Id())
	}
	args := []string{"delete", resource}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return run(kubectl(m, args...))
}

func resourceManifestRead(d *schema.ResourceData, m interface{}) error {
	resource, namespace, ok := resourceFromSelflink(d.Id())
	if !ok {
		return fmt.Errorf("invalid resource id: %s", d.Id())
	}

	args := []string{"get", "--ignore-not-found", resource}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}

	stdout := &bytes.Buffer{}
	cmd := kubectl(m, args...)
	cmd.Stdout = stdout
	if err := run(cmd); err != nil {
		return err
	}
	if strings.TrimSpace(stdout.String()) == "" {
		d.SetId("")
	}
	return nil
}
