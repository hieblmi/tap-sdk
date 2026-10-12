//go:build itest

package itest

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/btcsuite/btcd/psbt/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// interruptedPublisher drops a request or a successful response at the SDK
// transport boundary while all delivered requests use the real tapd daemon.
type interruptedPublisher struct {
	tapsdk.Client
	tapsdk.CustomAnchorWalletKitClient
	tapsdk.CustomAnchorCapabilityProvider

	beforeSend bool
}

func (c *interruptedPublisher) PublishAndLogTransferWithRequest(
	ctx context.Context, req *tapsdk.PublishAndLogTransferRequest) (
	*tapsdk.AssetPacket, error) {

	if c.beforeSend {
		return nil, context.DeadlineExceeded
	}
	_, err := c.CustomAnchorWalletKitClient.PublishAndLogTransferWithRequest(
		ctx, req,
	)
	if err != nil {
		return nil, err
	}
	return nil, context.DeadlineExceeded
}

// exercisePublicationRecovery tests the actual daemon's publication boundary
// before the externally broadcast anchor is visible on chain.
func exercisePublicationRecovery(t *testing.T, ctx context.Context,
	h *TestHarness, sealed *tapsdk.CustomAnchorTransferPackage,
	finalPSBT []byte) *tapsdk.AssetPacket {

	t.Helper()
	packet := decodeCustomAnchorITestPSBT(t, finalPSBT)
	tx, err := psbt.Extract(packet)
	require.NoError(t, err)
	list := func() []*tapsdk.AssetTransfer {
		transfers, err := h.AliceClient.ListTransfers(ctx,
			&tapsdk.ListTransfersRequest{
				AnchorTxid: tx.TxHash().String(),
			},
		)
		require.NoError(t, err)
		return transfers
	}

	walletKit := h.AliceClient.(tapsdk.CustomAnchorWalletKitClient)
	capabilities := h.AliceClient.(tapsdk.CustomAnchorCapabilityProvider)
	interrupted := &interruptedPublisher{
		Client:                         h.AliceClient,
		CustomAnchorWalletKitClient:    walletKit,
		CustomAnchorCapabilityProvider: capabilities,
		beforeSend:                     true,
	}
	wallet := tapsdk.NewWallet(interrupted, tapsdk.NetworkRegtest)
	_, err = wallet.PublishCustomAnchorTransfer(ctx, sealed, finalPSBT)
	var attempt *tapsdk.CustomAnchorPublishAttemptError
	require.ErrorAs(t, err, &attempt)
	require.True(t, attempt.OutcomeUnknown)
	require.Empty(t, list())

	// A rejected request must not prevent a valid retry.
	_, err = interrupted.CustomAnchorWalletKitClient.
		PublishAndLogTransferWithRequest(ctx,
			&tapsdk.PublishAndLogTransferRequest{
				AnchorPsbt:   finalPSBT,
				VirtualPsbts: [][]byte{{1}},
			},
		)
	require.Error(t, err)
	require.Empty(t, list())

	// Race the first successful submission with retries. One caller loses
	// its response, but the daemon must commit exactly one transfer.
	interrupted.beforeSend = false
	start := make(chan struct{})
	results := make(chan error, 5)
	for range 5 {
		go func() {
			<-start
			_, err := h.AliceWallet.PublishCustomAnchorTransfer(
				ctx, sealed, finalPSBT,
			)
			results <- err
		}()
	}
	lostResponse := make(chan error, 1)
	go func() {
		<-start
		_, err := wallet.PublishCustomAnchorTransfer(ctx, sealed, finalPSBT)
		lostResponse <- err
	}()
	close(start)
	for range 5 {
		require.NoError(t, <-results)
	}
	err = <-lostResponse
	require.ErrorAs(t, err, &attempt)
	require.True(t, attempt.OutcomeUnknown)
	require.Len(t, list(), 1)

	// A process restart discards all in-memory state. The
	// same request must still return the transfer stored before restart.
	output, err := exec.CommandContext(
		ctx, "docker", "restart", "tap-sdk-tapd-alice",
	).CombinedOutput()
	require.NoError(t, err, string(output))
	h.WaitForSync(t, ctx, h.AliceClient, 90*time.Second)
	logged, err := h.AliceWallet.PublishCustomAnchorTransfer(
		ctx, sealed, finalPSBT,
	)
	require.NoError(t, err)
	require.Len(t, list(), 1)

	return logged
}
