// Package route53 implements deploy.DNS on AWS Route 53: A records with a
// short TTL, since the cloud provider reuses addresses.
package route53

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
)

// ttl is short so a recreated environment's new address is seen quickly.
const ttl = 60

// ErrNoCredentials: nothing in the environment or ~/.aws to sign with.
var ErrNoCredentials = errors.New("no AWS credentials (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY)")

// DNS is a deploy.DNS on one hosted zone.
type DNS struct {
	c      *route53.Client
	zoneID string
	zone   string // the zone's name without the trailing dot
}

// New connects with the SDK's default credential chain and finds the hosted
// zone for domain: the zone whose name is domain's longest suffix, so
// gobank.example.com is served by the example.com zone.
func New(ctx context.Context, domain string) (*DNS, error) {
	// Not on EC2: skip the metadata service and its timeouts.
	cfg, err := config.LoadDefaultConfig(ctx, config.WithEC2IMDSClientEnableState(imds.ClientDisabled))
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1" // Route 53 is global; the SDK still wants one
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, ErrNoCredentials
	}
	d := &DNS{c: route53.NewFromConfig(cfg)}
	if err := d.findZone(ctx, domain); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *DNS) findZone(ctx context.Context, domain string) error {
	p := route53.NewListHostedZonesPaginator(d.c, &route53.ListHostedZonesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list hosted zones: %w", err)
		}
		for _, z := range page.HostedZones {
			name := strings.TrimSuffix(aws.ToString(z.Name), ".")
			if (domain == name || strings.HasSuffix(domain, "."+name)) && len(name) > len(d.zone) {
				d.zone, d.zoneID = name, aws.ToString(z.Id)
			}
		}
	}
	if d.zoneID == "" {
		return fmt.Errorf("no hosted zone for %s in this AWS account", domain)
	}
	return nil
}

// Zone is the hosted zone's name.
func (d *DNS) Zone() string { return d.zone }

func (d *DNS) Set(ctx context.Context, name, ip string) error {
	return d.change(ctx, types.ChangeActionUpsert, name, ip)
}

func (d *DNS) Delete(ctx context.Context, name string) error {
	ip, err := d.lookup(ctx, name)
	if err != nil || ip == "" {
		return err
	}
	return d.change(ctx, types.ChangeActionDelete, name, ip)
}

// lookup returns the A record's address, or empty when there is none.
func (d *DNS) lookup(ctx context.Context, name string) (string, error) {
	out, err := d.c.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(d.zoneID),
		StartRecordName: aws.String(name),
		StartRecordType: types.RRTypeA,
		MaxItems:        aws.Int32(1),
	})
	if err != nil {
		return "", fmt.Errorf("lookup %s: %w", name, err)
	}
	for _, rs := range out.ResourceRecordSets {
		if strings.TrimSuffix(aws.ToString(rs.Name), ".") == name && rs.Type == types.RRTypeA && len(rs.ResourceRecords) > 0 {
			return aws.ToString(rs.ResourceRecords[0].Value), nil
		}
	}
	return "", nil
}

func (d *DNS) change(ctx context.Context, action types.ChangeAction, name, ip string) error {
	_, err := d.c.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(d.zoneID),
		ChangeBatch: &types.ChangeBatch{Changes: []types.Change{{
			Action: action,
			ResourceRecordSet: &types.ResourceRecordSet{
				Name:            aws.String(name),
				Type:            types.RRTypeA,
				TTL:             aws.Int64(ttl),
				ResourceRecords: []types.ResourceRecord{{Value: aws.String(ip)}},
			},
		}}},
	})
	if err != nil {
		return fmt.Errorf("%s %s: %w", strings.ToLower(string(action)), name, err)
	}
	return nil
}

var _ deploy.DNS = (*DNS)(nil)
