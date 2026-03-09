package cmd

import (
	"fmt"

	"github.com/AlecAivazis/survey/v2"
	"github.com/evcc-io/evcc/charger/chargepoint"
	"github.com/evcc-io/evcc/util"
	"github.com/spf13/cobra"
)

var chargepointTokenCmd = &cobra.Command{
	Use:          "chargepoint-token",
	Short:        "Generate ChargePoint access tokens",
	RunE:         runChargepointToken,
	SilenceUsage: true,
}

func init() {
	rootCmd.AddCommand(chargepointTokenCmd)
}

func runChargepointToken(cmd *cobra.Command, args []string) error {
	var username, password string

	if err := survey.AskOne(&survey.Input{
		Message: "ChargePoint username:",
	}, &username, survey.WithValidator(survey.Required)); err != nil {
		return err
	}

	if err := survey.AskOne(&survey.Password{
		Message: "ChargePoint password:",
	}, &password, survey.WithValidator(survey.Required)); err != nil {
		return err
	}

	log := util.NewLogger("chargepoint")

	token, err := chargepoint.Login(log, username, password)
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("Add the following tokens to the charger config:")
	fmt.Println()
	fmt.Println("    type: chargepoint")
	fmt.Println("    accesstoken:", token.AccessToken)
	fmt.Println("    refreshtoken:", token.RefreshToken)

	return nil
}
