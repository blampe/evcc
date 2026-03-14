package cmd

import (
	"fmt"

	"github.com/AlecAivazis/survey/v2"
	"github.com/evcc-io/evcc/charger/chargepoint"
	"github.com/evcc-io/evcc/util"
	"github.com/kr/pretty"
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

	i, err := chargepoint.NewIdentity(log, username, password)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}

	err = i.Login()
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	pretty.Println("Region: ", i.Region)
	pretty.Println("User:   ", i.UserID)
	pretty.Println("Session:", i.SessionID)
	pretty.Println("SSO:    ", i.SSOSessionID)

	api := chargepoint.NewAPI(log, i)

	ids, err := api.HomeChargerIDs()
	if err != nil {
		return fmt.Errorf("chargers: %w", err)
	}
	if len(ids) == 0 {
		return fmt.Errorf("no chargers")
	}

	for _, id := range ids {
		stat, err := api.HomeChargerStatus(id)
		if err != nil {
			return fmt.Errorf("status: %w", err)
		}
		fmt.Println(id, stat)
		err = api.SetAmperageLimit(id, 48)
		if err != nil {
			return fmt.Errorf("amp limit: %w", err)
		}
		err = api.StartSession(id)
		if err != nil {
			return fmt.Errorf("start session: %w", err)
		}
		//err = api.StopSession(id)
		//if err != nil {
		//	return fmt.Errorf("stop session: %w", err)
		//}
	}

	userID, err := api.Account()
	if err != nil {
		return fmt.Errorf("userid: %w", err)
	}
	pretty.Println("User:   ", userID)

	return nil
}
