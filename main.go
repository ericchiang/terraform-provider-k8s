package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/ghodss/yaml"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/plugin"
	"github.com/hashicorp/terraform/terraform"
	"github.com/tdewolff/minify"
	minjson "github.com/tdewolff/minify/json"
	"log"
	"os/exec"
	"regexp"
	"strings"
)

func main() {
	plugin.Serve(&plugin.ServeOpts{
		ProviderFunc: func() terraform.ResourceProvider {
			return &schema.Provider{
				ResourcesMap: map[string]*schema.Resource{
					"k8s_manifest": resourceManifest(),
				},
			}
		},
	})
}

// This converts everything to minified JSON.  This is convenient because it
// makes the state smaller, which helps in case you are using the consul
// backend to store your data.  It also helps because it avoids subtle edge
// case bugs and formatting issues associated with slurping yaml in, then just
// spitting it out.  I have seen it cause problems when multi-line strings
// enter the picture.  Far better to normalize to json
func normalizeInput(input string) (string, error) {
	j, err := yaml.YAMLToJSON([]byte(input))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	r := strings.NewReader(string(j))
	m := minify.New()
	m.AddFuncRegexp(regexp.MustCompile("[/+]json$"), minjson.Minify)
	if err := m.Minify("application/json", &b, r); err != nil {
		return "", err
	}
	return string(b.String()), nil
}

func attemptNormalizeInput(input string) string {
	if normalized, err := normalizeInput(input); err != nil {
		return input
	} else {
		return normalized
	}
}

func resourceManifest() *schema.Resource {
	return &schema.Resource{
		Create: resourceManifestCreate,
		Read:   resourceManifestRead,
		Update: resourceManifestUpdate,
		Delete: resourceManifestDelete,
		Schema: map[string]*schema.Schema{
			"content": &schema.Schema{
				Type:     schema.TypeString,
				Required: true,
				StateFunc: func(thing interface{}) string {
					return attemptNormalizeInput(thing.(string))
				},
			},
		},
		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
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

func kubectlRun(subcommand, data string) (string, error) {
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}
	cmd := exec.Command("kubectl", subcommand, "-f", "-", "-o", "json")
	cmd.Stderr = stderr
	cmd.Stdout = stdout
	cmd.Stdin = strings.NewReader(data)
	if err := cmd.Run(); err != nil {
		if stderr.Len() == 0 {
			return "", fmt.Errorf("kubectl %s: %v", subcommand, err)
		}
		return "", fmt.Errorf("kubectl %s %v: %s", subcommand, err, stderr.Bytes())
	}
	log.Printf("%s\n", stdout.String())
	return "", nil
}

func resourceManifestCreate(d *schema.ResourceData, m interface{}) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(d.Get("content").(string))
	if err := run(cmd); err != nil {
		return err
	}

	stdout := &bytes.Buffer{}
	cmd = exec.Command("kubectl", "get", "-f", "-", "-o", "json")
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
	cmd := exec.Command("kubectl", "apply", "-f", "-")
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
	return run(exec.Command("kubectl", args...))
}

var objectMetadataReadError error = fmt.Errorf("Object definition's metadata could not be read")

func resourceManifestRead(d *schema.ResourceData, m interface{}) error {
	resource, namespace, ok := resourceFromSelflink(d.Id())
	if !ok {
		return fmt.Errorf("invalid resource id: %s", d.Id())
	}

	args := []string{"get", "--export=true", "-o", "yaml", "--ignore-not-found", resource}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}

	stdout := &bytes.Buffer{}
	cmd := exec.Command("kubectl", args...)
	cmd.Stdout = stdout
	if err := run(cmd); err != nil {
		return err
	}
	output := strings.TrimSpace(stdout.String())
	if output == "" {
		d.SetId("")
		return nil
	}
	var definition map[string]interface{}
	if err := yaml.Unmarshal([]byte(output), &definition); err != nil {
		return err
	}
	metadata, ok := definition["metadata"]
	if !ok {
		return fmt.Errorf("Object's metadata couldn't be found in: %v", definition)
	}
	metadataMap, ok := metadata.(map[interface{}]interface{})
	if !ok {
		return fmt.Errorf("Object's metadata wasn't of the expected type: %v", metadata)
	}
	annotations, ok := metadataMap["annotations"]
	if !ok {
		return fmt.Errorf("Object's annotations couldn't be found in: %v", metadataMap)
	}
	annotationsMap, ok := annotations.(map[interface{}]interface{})
	if !ok {
		return fmt.Errorf("Object's annotations wasn't of the expected type: %v", annotations)
	}
	rawConfiguration, ok := annotationsMap["kubectl.kubernetes.io/last-applied-configuration"]
	if !ok {
		return fmt.Errorf("Object's 'kubectl.kubernetes.io/last-applied-configuration' annotation couldn't be found in: %v", annotationsMap)
	}
	rawConfigurationStr, ok := rawConfiguration.(string)
	if !ok {
		return fmt.Errorf("Annotation 'kubectl.kubernetes.io/last-applied-configuration' was of an unexpected type: %v", rawConfiguration)
	}
	normalized, err := normalizeInput(rawConfigurationStr)
	if err != nil {
		return err
	}
	d.Set("content", normalized)
	return nil
}
