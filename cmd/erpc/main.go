package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/flair-sdk/erpc/erpc"
	"github.com/flair-sdk/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
)

func main() {
	logger := log.With().Logger()

	shutdown, err := erpc.Init(context.Background(), &logger, afero.NewOsFs(), os.Args)
	defer shutdown()

	if err != nil {
		logger.Error().Msgf("failed to start eRPC: %v", err)
		util.OsExit(util.ExitCodeERPCStartFailed)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	recvSig := <-sig
	logger.Warn().Msgf("caught signal: %v", recvSig)
}
