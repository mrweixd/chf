package abmf

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type sessionReceipt struct {
	Session string
	Result  receipt
}

func receiptHistory(accounts *mongo.Collection) *mongo.Collection {
	return accounts.Database().Collection(accounts.Name() + ".receipts")
}

func receiptID(account any, session string) bson.M {
	return bson.M{"_id": bson.D{{Key: "account", Value: account}, {Key: "session", Value: session}}}
}

// A slower writer may archive an older snapshot after a newer result. Never
// move the watermark backwards, including when retrying an interrupted migration.
func archiveReceipt(account any, r sessionReceipt) *mongo.UpdateOneModel {
	newer := bson.M{"$gt": bson.A{r.Result.Number, bson.M{"$ifNull": bson.A{"$result.number", -1}}}}
	update := mongo.Pipeline{bson.D{{Key: "$set", Value: bson.M{
		"result": bson.M{"$cond": bson.A{newer, bson.M{"$literal": r.Result}, "$result"}},
	}}}}
	return mongo.NewUpdateOneModel().SetFilter(receiptID(account, r.Session)).SetUpdate(update).SetUpsert(true)
}

// Drain legacy embedded receipts in batches. Remove entries only after the
// archive acknowledges them. The predicates protect concurrent migrators.
func migrateReceipts(ctx context.Context, accounts, history *mongo.Collection,
	account any, legacy map[string]receipt,
) error {
	const batchSize = 128
	var writes []mongo.WriteModel
	match := bson.M{"_id": account}
	unset := bson.M{}
	for session, r := range legacy {
		field := "chfReceipts." + session
		writes = append(writes, archiveReceipt(account, sessionReceipt{Session: session, Result: r}))
		match[field+".number"] = r.Number
		unset[field] = ""
		if len(writes) == batchSize {
			break
		}
	}
	if _, err := history.BulkWrite(ctx, writes); err != nil {
		return err
	}
	_, err := accounts.UpdateOne(ctx, match, bson.M{"$unset": unset})
	return err
}
