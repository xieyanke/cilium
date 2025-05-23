// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cmd

import (
	"os"

	"github.com/spf13/cobra"

	endpointApi "github.com/cilium/cilium/api/v1/client/endpoint"
	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/api"
	"github.com/cilium/cilium/pkg/command"
)

var lbls []string

// endpointGetCmd represents the endpoint_get command
var endpointGetCmd = &cobra.Command{
	Use:     "get ( <endpoint identifier> | -l <endpoint labels> ) ",
	Aliases: []string{"inspect, show"},
	Short:   "Display endpoint information",
	Example: `  cilium-dbg endpoint get 4598
  cilium-dbg endpoint get pod-name:default:foobar
  cilium-dbg endpoint get -l id.baz`,
	Run: func(cmd *cobra.Command, args []string) {
		if len(lbls) > 0 && len(args) > 0 {
			Usagef(cmd, "Cannot provide both endpoint ID and labels arguments concurrently")
		}
		var endpointInst []*models.Endpoint

		if len(lbls) > 0 {
			params := endpointApi.NewGetEndpointParams().WithLabels(lbls).WithTimeout(api.ClientTimeout)
			result, err := client.Endpoint.GetEndpoint(params)
			if err != nil {
				Fatalf("Cannot get endpoints for given list of labels %s: %s\n", lbls, err)
			}
			endpointInst = result.Payload
		} else {
			requireEndpointID(cmd, args)
			eID := args[0]
			result, err := client.EndpointGet(eID)
			if err != nil {
				Fatalf("Cannot get endpoint %s: %s\n", eID, err)
			}
			endpointInst = append(endpointInst, result)
		}

		if command.OutputOption() {
			if err := command.PrintOutput(endpointInst); err != nil {
				os.Exit(1)
			}
			return
		}
		command.ForceJSON()
		if err := command.PrintOutput(endpointInst); err != nil {
			os.Exit(1)
		}
	},
}

func init() {
	EndpointCmd.AddCommand(endpointGetCmd)
	endpointGetCmd.Flags().StringSliceVarP(&lbls, "labels", "l", []string{}, "list of labels")
	command.AddOutputOption(endpointGetCmd)
}
