package abmf

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/fiorix/go-diameter/diam/datatype"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	cd "github.com/free5gc/chf/ccs_diameter/datatype"
)

// Set CHF_TEST_MONGO_URI to enable the atomic accounting integration test.
func TestLostAccountAnswer(t *testing.T) {
	uri := os.Getenv("CHF_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("requires CHF_TEST_MONGO_URI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	require.NoError(t, err)
	defer func() { require.NoError(t, client.Disconnect(ctx)) }()
	c := client.Database("chf_test").Collection(uuid.NewString())
	defer func() { require.NoError(t, c.Drop(ctx)) }()
	for _, group := range []int64{1, 2} {
		_, err = c.InsertOne(ctx, bson.M{"ueId": "imsi-test", "ratingGroup": group, "quota": "100"})
		require.NoError(t, err)
	}
	request := func(group uint32) *cd.AccountDebitRequest {
		return &cd.AccountDebitRequest{
			SessionId:       "test-release",
			SubscriptionId:  &cd.SubscriptionId{SubscriptionIdType: cd.END_USER_IMSI, SubscriptionIdData: "test"},
			RequestedAction: cd.DIRECT_DEBITING, CcRequestType: cd.TERMINATION_REQUEST,
			CcRequestNumber: datatype.Unsigned32(group),
			MultipleServicesCreditControl: &cd.MultipleServicesCreditControl{
				RatingGroup:     datatype.Unsigned32(group),
				UsedServiceUnit: &cd.UsedServiceUnit{CCTotalOctets: 10},
			},
		}
	}
	balance := func(group int64, want string) {
		var got struct{ Quota string }
		require.NoError(t, c.FindOne(ctx, bson.M{"ratingGroup": group}).Decode(&got))
		require.Equal(t, want, got.Quota)
	}
	_, err = account(ctx, c, request(1))
	require.NoError(t, err)
	b := request(2)
	_, err = account(ctx, c, b) // Commit B and discard its answer.
	require.NoError(t, err)
	replay, err := account(ctx, c, b)
	require.NoError(t, err)
	require.EqualValues(t, 90, replay.RemainingBalance.UnitValue.ValueDigits)
	balance(1, "90")
	balance(2, "90")
	b.MultipleServicesCreditControl.UsedServiceUnit.CCTotalOctets = 11
	_, err = account(ctx, c, b)
	require.Error(t, err)
	balance(2, "90")
	b.CcRequestNumber++
	b.RequestedAction = cd.REFUND_ACCOUNT
	b.MultipleServicesCreditControl.RequestedServiceUnit = &cd.RequestedServiceUnit{CCTotalOctets: 5}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := account(ctx, c, b); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	balance(2, "95")
	_, err = account(ctx, c, request(2))
	require.Error(t, err)
	balance(2, "95")
}
