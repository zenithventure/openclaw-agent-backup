package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DynamoStore implements DataStore using DynamoDB (for Lambda deployment).
type DynamoStore struct {
	client       *dynamodb.Client
	agentsTable  string
	backupsTable string
	retentionDays int
}

// DynamoDB item schemas

type dynamoAgent struct {
	ID              string `dynamodbav:"id"`
	Name            string `dynamodbav:"name"`
	Hostname        string `dynamodbav:"hostname"`
	OS              string `dynamodbav:"os"`
	Arch            string `dynamodbav:"arch"`
	OpenClawVersion string `dynamodbav:"openclaw_version"`
	Fingerprint     string `dynamodbav:"fingerprint"`
	EncryptTool     string `dynamodbav:"encrypt_tool"`
	PublicKey       string `dynamodbav:"public_key"`
	TokenHash       string `dynamodbav:"token_hash"`
	QuotaBytes      int64  `dynamodbav:"quota_bytes"`
	UsedBytes       int64  `dynamodbav:"used_bytes"`
	CreatedAt       string `dynamodbav:"created_at"`
}

type dynamoBackup struct {
	AgentID         string `dynamodbav:"agent_id"`
	Timestamp       string `dynamodbav:"timestamp"`
	EncryptedBytes  int64  `dynamodbav:"encrypted_bytes"`
	SourceFileCount int64  `dynamodbav:"source_file_count"`
	EncryptedSHA256 string `dynamodbav:"encrypted_sha256"`
	S3Key           string `dynamodbav:"s3_key"`
	ManifestS3Key   string `dynamodbav:"manifest_s3_key"`
	CreatedAt       string `dynamodbav:"created_at"`
	ExpiresAt       int64  `dynamodbav:"expires_at"` // TTL attribute
}

func NewDynamoStore(ctx context.Context, cfg *Config) (*DynamoStore, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.S3Region),
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	var clientOpts []func(*dynamodb.Options)
	if cfg.DynamoEndpoint != "" {
		clientOpts = append(clientOpts, func(o *dynamodb.Options) {
			o.BaseEndpoint = aws.String(cfg.DynamoEndpoint)
		})
	}

	client := dynamodb.NewFromConfig(awsCfg, clientOpts...)

	return &DynamoStore{
		client:        client,
		agentsTable:   cfg.DynamoAgentsTable,
		backupsTable:  cfg.DynamoBackupsTable,
		retentionDays: cfg.RetentionDays,
	}, nil
}

func (s *DynamoStore) Close() error {
	return nil // DynamoDB client doesn't need closing
}

// ---------------------------------------------------------------------------
// Agent operations
// ---------------------------------------------------------------------------

func (s *DynamoStore) CreateAgent(a *Agent, tokenHash string) error {
	item := dynamoAgent{
		ID:              a.ID,
		Name:            a.Name,
		Hostname:        a.Hostname,
		OS:              a.OS,
		Arch:            a.Arch,
		OpenClawVersion: a.OpenClawVersion,
		Fingerprint:     a.Fingerprint,
		EncryptTool:     a.EncryptTool,
		PublicKey:        a.PublicKey,
		TokenHash:       tokenHash,
		QuotaBytes:      a.QuotaBytes,
		UsedBytes:       0,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("marshal agent: %w", err)
	}

	_, err = s.client.PutItem(context.Background(), &dynamodb.PutItemInput{
		TableName: aws.String(s.agentsTable),
		Item:      av,
	})
	return err
}

func (s *DynamoStore) LookupAgentByToken(token string) (*Agent, error) {
	h := HashToken(token)

	// Query the GSI on token_hash
	out, err := s.client.Query(context.Background(), &dynamodb.QueryInput{
		TableName:              aws.String(s.agentsTable),
		IndexName:              aws.String("token-hash-index"),
		KeyConditionExpression: aws.String("token_hash = :th"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":th": &types.AttributeValueMemberS{Value: h},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("query token GSI: %w", err)
	}

	if len(out.Items) == 0 {
		return nil, nil
	}

	return unmarshalAgent(out.Items[0])
}

func (s *DynamoStore) GetAgent(id string) (*Agent, error) {
	out, err := s.client.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName: aws.String(s.agentsTable),
		Key: map[string]types.AttributeValue{
			"id": &types.AttributeValueMemberS{Value: id},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get agent: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	return unmarshalAgent(out.Item)
}

func (s *DynamoStore) RotateAgentToken(agentID, newTokenHash string) error {
	_, err := s.client.UpdateItem(context.Background(), &dynamodb.UpdateItemInput{
		TableName: aws.String(s.agentsTable),
		Key: map[string]types.AttributeValue{
			"id": &types.AttributeValueMemberS{Value: agentID},
		},
		UpdateExpression: aws.String("SET token_hash = :th"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":th": &types.AttributeValueMemberS{Value: newTokenHash},
		},
	})
	return err
}

func (s *DynamoStore) UpdateUsedBytes(agentID string) error {
	// In DynamoDB we recalculate by querying backups
	_, totalBytes, err := s.CountBackups(agentID)
	if err != nil {
		return err
	}

	_, err = s.client.UpdateItem(context.Background(), &dynamodb.UpdateItemInput{
		TableName: aws.String(s.agentsTable),
		Key: map[string]types.AttributeValue{
			"id": &types.AttributeValueMemberS{Value: agentID},
		},
		UpdateExpression: aws.String("SET used_bytes = :ub"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":ub": &types.AttributeValueMemberN{Value: strconv.FormatInt(totalBytes, 10)},
		},
	})
	return err
}

// ---------------------------------------------------------------------------
// Backup operations
// ---------------------------------------------------------------------------

func (s *DynamoStore) CreateBackup(b *Backup) error {
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(s.retentionDays*24) * time.Hour)

	item := dynamoBackup{
		AgentID:         b.AgentID,
		Timestamp:       b.Timestamp,
		EncryptedBytes:  b.EncryptedBytes,
		SourceFileCount: b.SourceFileCount,
		EncryptedSHA256: b.EncryptedSHA256,
		S3Key:           b.S3Key,
		ManifestS3Key:   b.ManifestS3Key,
		CreatedAt:       now.Format(time.RFC3339),
		ExpiresAt:       expiresAt.Unix(),
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("marshal backup: %w", err)
	}

	_, err = s.client.PutItem(context.Background(), &dynamodb.PutItemInput{
		TableName: aws.String(s.backupsTable),
		Item:      av,
	})
	if err != nil {
		return err
	}

	return s.UpdateUsedBytes(b.AgentID)
}

func (s *DynamoStore) ListBackups(agentID string, limit int) ([]Backup, error) {
	if limit <= 0 {
		limit = 100
	}

	out, err := s.client.Query(context.Background(), &dynamodb.QueryInput{
		TableName:              aws.String(s.backupsTable),
		KeyConditionExpression: aws.String("agent_id = :aid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":aid": &types.AttributeValueMemberS{Value: agentID},
		},
		ScanIndexForward: aws.Bool(false), // newest first
		Limit:            aws.Int32(int32(limit)),
	})
	if err != nil {
		return nil, fmt.Errorf("query backups: %w", err)
	}

	backups := make([]Backup, 0, len(out.Items))
	for _, item := range out.Items {
		b, err := unmarshalBackup(item)
		if err != nil {
			return nil, err
		}
		backups = append(backups, *b)
	}
	return backups, nil
}

func (s *DynamoStore) CountBackups(agentID string) (int, int64, error) {
	// Query all backups for this agent to sum bytes
	// For large datasets, consider a counter attribute on the agent item
	out, err := s.client.Query(context.Background(), &dynamodb.QueryInput{
		TableName:              aws.String(s.backupsTable),
		KeyConditionExpression: aws.String("agent_id = :aid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":aid": &types.AttributeValueMemberS{Value: agentID},
		},
		ProjectionExpression: aws.String("encrypted_bytes"),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("count backups: %w", err)
	}

	var totalBytes int64
	for _, item := range out.Items {
		if v, ok := item["encrypted_bytes"]; ok {
			if n, ok := v.(*types.AttributeValueMemberN); ok {
				b, _ := strconv.ParseInt(n.Value, 10, 64)
				totalBytes += b
			}
		}
	}

	return int(out.Count), totalBytes, nil
}

func (s *DynamoStore) GetBackup(agentID, timestamp string) (*Backup, error) {
	out, err := s.client.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName: aws.String(s.backupsTable),
		Key: map[string]types.AttributeValue{
			"agent_id":  &types.AttributeValueMemberS{Value: agentID},
			"timestamp": &types.AttributeValueMemberS{Value: timestamp},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get backup: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	return unmarshalBackup(out.Item)
}

func (s *DynamoStore) DeleteBackup(agentID, timestamp string) (*Backup, error) {
	// Get first so we can return the deleted item
	b, err := s.GetBackup(agentID, timestamp)
	if err != nil || b == nil {
		return nil, err
	}

	_, err = s.client.DeleteItem(context.Background(), &dynamodb.DeleteItemInput{
		TableName: aws.String(s.backupsTable),
		Key: map[string]types.AttributeValue{
			"agent_id":  &types.AttributeValueMemberS{Value: agentID},
			"timestamp": &types.AttributeValueMemberS{Value: timestamp},
		},
	})
	if err != nil {
		return nil, err
	}

	_ = s.UpdateUsedBytes(agentID)
	return b, nil
}

func (s *DynamoStore) DeleteAllBackups(agentID string) ([]Backup, error) {
	backups, err := s.ListBackups(agentID, 10000)
	if err != nil {
		return nil, err
	}

	// DynamoDB doesn't have bulk delete — delete one by one
	for _, b := range backups {
		_, _ = s.client.DeleteItem(context.Background(), &dynamodb.DeleteItemInput{
			TableName: aws.String(s.backupsTable),
			Key: map[string]types.AttributeValue{
				"agent_id":  &types.AttributeValueMemberS{Value: b.AgentID},
				"timestamp": &types.AttributeValueMemberS{Value: b.Timestamp},
			},
		})
	}

	_ = s.UpdateUsedBytes(agentID)
	return backups, nil
}

// ---------------------------------------------------------------------------
// Unmarshal helpers
// ---------------------------------------------------------------------------

func unmarshalAgent(item map[string]types.AttributeValue) (*Agent, error) {
	var da dynamoAgent
	if err := attributevalue.UnmarshalMap(item, &da); err != nil {
		return nil, fmt.Errorf("unmarshal agent: %w", err)
	}

	createdAt, _ := time.Parse(time.RFC3339, da.CreatedAt)

	return &Agent{
		ID:              da.ID,
		Name:            da.Name,
		Hostname:        da.Hostname,
		OS:              da.OS,
		Arch:            da.Arch,
		OpenClawVersion: da.OpenClawVersion,
		Fingerprint:     da.Fingerprint,
		EncryptTool:     da.EncryptTool,
		PublicKey:        da.PublicKey,
		QuotaBytes:      da.QuotaBytes,
		UsedBytes:       da.UsedBytes,
		CreatedAt:       createdAt,
	}, nil
}

func unmarshalBackup(item map[string]types.AttributeValue) (*Backup, error) {
	var db dynamoBackup
	if err := attributevalue.UnmarshalMap(item, &db); err != nil {
		return nil, fmt.Errorf("unmarshal backup: %w", err)
	}

	createdAt, _ := time.Parse(time.RFC3339, db.CreatedAt)

	return &Backup{
		AgentID:         db.AgentID,
		Timestamp:       db.Timestamp,
		EncryptedBytes:  db.EncryptedBytes,
		SourceFileCount: db.SourceFileCount,
		EncryptedSHA256: db.EncryptedSHA256,
		S3Key:           db.S3Key,
		ManifestS3Key:   db.ManifestS3Key,
		CreatedAt:       createdAt,
	}, nil
}
