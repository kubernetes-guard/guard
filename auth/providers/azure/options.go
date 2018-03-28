package azure

import (
	"fmt"
	"os"

	"github.com/appscode/go/types"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	"k8s.io/api/apps/v1beta1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type Options struct {
	ClientID     string
	ClientSecret string
	TenantID     string
}

func NewOptions() Options {
	return Options{
		ClientSecret: os.Getenv("AZURE_CLIENT_SECRET"),
	}
}

func (o *Options) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.ClientID, "azure.client-id", o.ClientID, "MS Graph application client ID to use")
	fs.StringVar(&o.TenantID, "azure.tenant-id", o.TenantID, "MS Graph application tenant id to use")
}

func (o *Options) Validate() []error {
	var errs []error
	if o.ClientSecret == "" {
		errs = append(errs, errors.New("environment variable AZURE_CLIENT_SECRET must be set"))
	}
	if o.ClientID == "" {
		errs = append(errs, errors.New("azure.client-id must be non empty"))
	}
	if o.TenantID == "" {
		errs = append(errs, errors.New("azure.tenant-id must be non empty"))
	}
	return errs
}

func (o Options) Apply(d *v1beta1.Deployment) (extraObjs []runtime.Object, err error) {
	// create auth secret
	authSecret := &core.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "guard-azure-auth",
			Namespace: d.Namespace,
			Labels:    d.Labels,
		},
		Data: map[string][]byte{
			"client-secret": []byte(o.ClientSecret),
		},
	}
	extraObjs = append(extraObjs, authSecret)

	// mount auth secret into deployment
	volMount := core.VolumeMount{
		Name:      authSecret.Name,
		MountPath: "/etc/guard/auth/azure",
	}
	d.Spec.Template.Spec.Containers[0].VolumeMounts = append(d.Spec.Template.Spec.Containers[0].VolumeMounts, volMount)

	vol := core.Volume{
		Name: authSecret.Name,
		VolumeSource: core.VolumeSource{
			Secret: &core.SecretVolumeSource{
				SecretName:  authSecret.Name,
				DefaultMode: types.Int32P(0555),
			},
		},
	}
	d.Spec.Template.Spec.Volumes = append(d.Spec.Template.Spec.Volumes, vol)

	// use auth secret in container[0] args
	d.Spec.Template.Spec.Containers[0].Env = append(d.Spec.Template.Spec.Containers[0].Env, core.EnvVar{
		Name: "AZURE_CLIENT_SECRET",
		ValueFrom: &core.EnvVarSource{
			SecretKeyRef: &core.SecretKeySelector{
				LocalObjectReference: core.LocalObjectReference{
					Name: authSecret.Name,
				},
				Key: "client-secret",
			},
		},
	})

	args := d.Spec.Template.Spec.Containers[0].Args
	if o.ClientID != "" {
		args = append(args, fmt.Sprintf("--azure.client-id=%s", o.ClientID))
	}
	if o.TenantID != "" {
		args = append(args, fmt.Sprintf("--azure.tenant-id=%s", o.TenantID))
	}

	d.Spec.Template.Spec.Containers[0].Args = args

	return extraObjs, nil
}
