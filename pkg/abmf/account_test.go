package abmf

import (
	"context"
	"crypto/sha256"
	"fmt"
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
func testAccounts(t *testing.T) (context.Context, *mongo.Collection) {
	t.Helper()
	uri := os.Getenv("CHF_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("requires CHF_TEST_MONGO_URI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(ctx)) })
	c := client.Database("chf_test").Collection(uuid.NewString())
	t.Cleanup(func() { require.NoError(t, receiptHistory(c).Drop(ctx)); require.NoError(t, c.Drop(ctx)) })
	return ctx, c
}

func TestLostAccountAnswer(t *testing.T) {
	ctx, c := testAccounts(t)
	for _, group := range []int64{1, 2} {
		_, err := c.InsertOne(ctx, bson.M{"ueId": "imsi-test", "ratingGroup": group, "quota": "100"})
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
	_, err := account(ctx, c, request(1))
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

func TestBoundedAccountReceipts(t *testing.T) {
	ctx, c := testAccounts(t)
	legacy := map[string]receipt{}
	for i := range 160 {
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("old-%d", i))))
		legacy[key] = receipt{
			Number: 3, Kind: int32(cd.TERMINATION_REQUEST), Action: int32(cd.DIRECT_DEBITING),
			Amount: 1, After: 1234,
		}
	}
	_, err := c.InsertOne(ctx, bson.M{
		"_id": "acct", "ueId": "imsi-test", "ratingGroup": 1, "quota": "1000", "chfReceipts": legacy,
	})
	require.NoError(t, err)
	r := &cd.AccountDebitRequest{
		SessionId: "old-0", CcRequestNumber: 3,
		SubscriptionId:  &cd.SubscriptionId{SubscriptionIdType: cd.END_USER_IMSI, SubscriptionIdData: "test"},
		RequestedAction: cd.DIRECT_DEBITING, CcRequestType: cd.TERMINATION_REQUEST,
		MultipleServicesCreditControl: &cd.MultipleServicesCreditControl{
			RatingGroup: 1, UsedServiceUnit: &cd.UsedServiceUnit{CCTotalOctets: 1},
		},
	}
	replay, err := account(ctx, c, r)
	require.NoError(t, err)
	require.EqualValues(t, 1234, replay.RemainingBalance.UnitValue.ValueDigits)
	var firstSize int
	for i := range 200 {
		r.SessionId = datatype.UTF8String(fmt.Sprintf("new-%d", i))
		_, err = account(ctx, c, r)
		require.NoError(t, err)
		raw, e := c.FindOne(ctx, bson.M{"_id": "acct"}).Raw()
		require.NoError(t, e)
		if i == 0 {
			firstSize = len(raw)
		} else {
			require.LessOrEqual(t, len(raw), firstSize)
		}
	}
	r.SessionId = "new-0"
	replay, err = account(ctx, c, r)
	require.NoError(t, err)
	require.EqualValues(t, 999, replay.RemainingBalance.UnitValue.ValueDigits)
	// A writer which read an older snapshot before eviction can finish late.
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(r.SessionId)))
	_, err = receiptHistory(c).BulkWrite(ctx, []mongo.WriteModel{archiveReceipt("acct", sessionReceipt{
		Session: key, Result: receipt{Number: 2, After: 4321},
	})})
	require.NoError(t, err)
	replay, err = account(ctx, c, r)
	require.NoError(t, err)
	require.EqualValues(t, 999, replay.RemainingBalance.UnitValue.ValueDigits)
	r.CcRequestNumber--
	_, err = account(ctx, c, r)
	require.Error(t, err)
	var doc struct {
		Quota  string
		Legacy map[string]receipt `bson:"chfReceipts"`
	}
	require.NoError(t, c.FindOne(ctx, bson.M{"_id": "acct"}).Decode(&doc))
	require.Empty(t, doc.Legacy)
	require.Equal(t, "800", doc.Quota)
	count, err := receiptHistory(c).CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	require.EqualValues(t, 359, count)
}
