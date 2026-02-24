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
	client          *dynamodb.Client
	agentsTable     string
	backupsTable    string
	retentionDays   int
	deleteGraceHours int
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
	Status          string `dynamodbav:"status"`
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
	ExpiresAt       int64  `dynamodbav:"expires_at"`    // TTL attribute
	DeletedAt       string `dynamodbav:"deleted_at,omitempty"`
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
		client:           client,
		agentsTable:      cfg.DynamoAgentsTable,
		backupsTable:     cfg.DynamoBackupsTable,
		retentionDays:    cfg.RetentionDays,
		deleteGraceHours: cfg.DeleteGraceHours,
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
		Status:          a.Status,
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

func (s *DynamoStore) ListAgents(status string) ([]Agent, error) {
	input := &dynamodb.ScanInput{
		TableName: aws.String(s.agentsTable),
	}

	if status != "" {
		input.FilterExpression = aws.String("#s = :s")
		input.ExpressionAttributeNames = map[string]string{
			"#s": "status",
		}
		input.ExpressionAttributeValues = map[string]types.AttributeValue{
			":s": &types.AttributeValueMemberS{Value: status},
		}
	}

	out, err := s.client.Scan(context.Background(), input)
	if err != nil {
		return nil, fmt.Errorf("scan agents: %w", err)
	}

	agents := make([]Agent, 0, len(out.Items))
	for _, item := range out.Items {
		a, err := unmarshalAgent(item)
		if err != nil {
			return nil, err
		}
		agents = append(agents, *a)
	}
	return agents, nil
}

func (s *DynamoStore) CountAgentsByStatus(status string) (int, error) {
	out, err := s.client.Scan(context.Background(), &dynamodb.ScanInput{
		TableName:        aws.String(s.agentsTable),
		FilterExpression: aws.String("#s = :s"),
		ExpressionAttributeNames: map[string]string{
			"#s": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":s": &types.AttributeValueMemberS{Value: status},
		},
		Select: types.SelectCount,
	})
	if err != nil {
		return 0, fmt.Errorf("count agents by status: %w", err)
	}
	return int(out.Count), nil
}

func (s *DynamoStore) UpdateAgentStatus(id, status string) error {
	_, err := s.client.UpdateItem(context.Background(), &dynamodb.UpdateItemInput{
		TableName: aws.String(s.agentsTable),
		Key: map[string]types.AttributeValue{
			"id": &types.AttributeValueMemberS{Value: id},
		},
		UpdateExpression: aws.String("SET #s = :s"),
		ExpressionAttributeNames: map[string]string{
			"#s": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":s": &types.AttributeValueMemberS{Value: status},
		},
		ConditionExpression: aws.String("attribute_exists(id)"),
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
		FilterExpression:       aws.String("attribute_not_exists(deleted_at) OR deleted_at = :empty"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":aid":   &types.AttributeValueMemberS{Value: agentID},
			":empty": &types.AttributeValueMemberS{Value: ""},
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
	// Query all non-deleted backups for this agent to sum bytes
	out, err := s.client.Query(context.Background(), &dynamodb.QueryInput{
		TableName:              aws.String(s.backupsTable),
		KeyConditionExpression: aws.String("agent_id = :aid"),
		FilterExpression:       aws.String("attribute_not_exists(deleted_at) OR deleted_at = :empty"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":aid":   &types.AttributeValueMemberS{Value: agentID},
			":empty": &types.AttributeValueMemberS{Value: ""},
		},
		ProjectionExpression: aws.String("encrypted_bytes"),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("count backups: %w", err)
	}

	var totalBytes int64
	count := 0
	for _, item := range out.Items {
		count++
		if v, ok := item["encrypted_bytes"]; ok {
			if n, ok := v.(*types.AttributeValueMemberN); ok {
				b, _ := strconv.ParseInt(n.Value, 10, 64)
				totalBytes += b
			}
		}
	}

	return count, totalBytes, nil
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
	b, err := unmarshalBackup(out.Item)
	if err != nil {
		return nil, err
	}
	if b.DeletedAt != nil {
		return nil, nil // treat soft-deleted as not found
	}
	return b, nil
}

func (s *DynamoStore) DeleteBackup(agentID, timestamp string) (*Backup, error) {
	// Get first so we can return the deleted item
	b, err := s.GetBackup(agentID, timestamp)
	if err != nil || b == nil {
		return nil, err
	}

	now := time.Now().UTC()
	graceExpiry := now.Add(time.Duration(s.deleteGraceHours) * time.Hour)

	_, err = s.client.UpdateItem(context.Background(), &dynamodb.UpdateItemInput{
		TableName: aws.String(s.backupsTable),
		Key: map[string]types.AttributeValue{
			"agent_id":  &types.AttributeValueMemberS{Value: agentID},
			"timestamp": &types.AttributeValueMemberS{Value: timestamp},
		},
		UpdateExpression: aws.String("SET deleted_at = :da, expires_at = :ea"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":da": &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
			":ea": &types.AttributeValueMemberN{Value: strconv.FormatInt(graceExpiry.Unix(), 10)},
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

	now := time.Now().UTC()
	graceExpiry := now.Add(time.Duration(s.deleteGraceHours) * time.Hour)

	// Soft-delete each backup
	for _, b := range backups {
		_, _ = s.client.UpdateItem(context.Background(), &dynamodb.UpdateItemInput{
			TableName: aws.String(s.backupsTable),
			Key: map[string]types.AttributeValue{
				"agent_id":  &types.AttributeValueMemberS{Value: b.AgentID},
				"timestamp": &types.AttributeValueMemberS{Value: b.Timestamp},
			},
			UpdateExpression: aws.String("SET deleted_at = :da, expires_at = :ea"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":da": &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
				":ea": &types.AttributeValueMemberN{Value: strconv.FormatInt(graceExpiry.Unix(), 10)},
			},
		})
	}

	_ = s.UpdateUsedBytes(agentID)
	return backups, nil
}

func (s *DynamoStore) UndeleteBackup(agentID, timestamp string) error {
	// Get the raw item (including soft-deleted)
	out, err := s.client.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName: aws.String(s.backupsTable),
		Key: map[string]types.AttributeValue{
			"agent_id":  &types.AttributeValueMemberS{Value: agentID},
			"timestamp": &types.AttributeValueMemberS{Value: timestamp},
		},
	})
	if err != nil {
		return fmt.Errorf("get backup for undelete: %w", err)
	}
	if out.Item == nil {
		return fmt.Errorf("backup not found or not deleted")
	}

	// Check if it's actually soft-deleted
	b, err := unmarshalBackup(out.Item)
	if err != nil {
		return err
	}
	if b.DeletedAt == nil {
		return fmt.Errorf("backup not found or not deleted")
	}

	// Restore: remove deleted_at, reset expires_at to original retention
	newExpiry := time.Now().UTC().Add(time.Duration(s.retentionDays*24) * time.Hour)
	_, err = s.client.UpdateItem(context.Background(), &dynamodb.UpdateItemInput{
		TableName: aws.String(s.backupsTable),
		Key: map[string]types.AttributeValue{
			"agent_id":  &types.AttributeValueMemberS{Value: agentID},
			"timestamp": &types.AttributeValueMemberS{Value: timestamp},
		},
		UpdateExpression: aws.String("REMOVE deleted_at SET expires_at = :ea"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":ea": &types.AttributeValueMemberN{Value: strconv.FormatInt(newExpiry.Unix(), 10)},
		},
	})
	if err != nil {
		return err
	}

	_ = s.UpdateUsedBytes(agentID)
	return nil
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

	// Backwards compat: treat empty/missing status as "active"
	status := da.Status
	if status == "" {
		status = "active"
	}

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
		Status:          status,
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

	b := &Backup{
		AgentID:         db.AgentID,
		Timestamp:       db.Timestamp,
		EncryptedBytes:  db.EncryptedBytes,
		SourceFileCount: db.SourceFileCount,
		EncryptedSHA256: db.EncryptedSHA256,
		S3Key:           db.S3Key,
		ManifestS3Key:   db.ManifestS3Key,
		CreatedAt:       createdAt,
	}

	if db.DeletedAt != "" {
		t, err := time.Parse(time.RFC3339, db.DeletedAt)
		if err == nil {
			b.DeletedAt = &t
		}
	}

	return b, nil
}
