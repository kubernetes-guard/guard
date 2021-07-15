/*
Copyright The Guard Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package commands

import (
	"go.kubeguard.dev/guard/server"

	"github.com/spf13/cobra"
	"gomodules.xyz/flags"
	"k8s.io/klog/v2"
)

func NewCmdRun() *cobra.Command {
	o := server.NewAuthRecommendedOptions()
	ao := server.NewAuthzRecommendedOptions()
	srv := server.Server{
		AuthRecommendedOptions:  o,
		AuthzRecommendedOptions: ao,
	}
	cmd := &cobra.Command{
		Use:               "run",
		Short:             "Run server",
		DisableAutoGenTag: true,
		PreRun: func(c *cobra.Command, args []string) {
			flags.PrintFlags(c.Flags())
		},
		Run: func(cmd *cobra.Command, args []string) {
			if !srv.AuthRecommendedOptions.SecureServing.UseTLS() {
				klog.Fatalln("Guard server must use SSL.")
			}
			srv.ListenAndServe()
		},
	}
	srv.AddFlags(cmd.Flags())
	return cmd
}
