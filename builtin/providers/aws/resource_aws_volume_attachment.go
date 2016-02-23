package aws

import (
	"bytes"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/hashicorp/terraform/helper/hashcode"
	"github.com/hashicorp/terraform/helper/resource"
	"github.com/hashicorp/terraform/helper/schema"
)

func resourceAwsVolumeAttachment() *schema.Resource {
	return &schema.Resource{
		Create: resourceAwsVolumeAttachmentCreate,
		Read:   resourceAwsVolumeAttachmentRead,
		Delete: resourceAwsVolumeAttachmentDelete,

		Schema: map[string]*schema.Schema{
			"device_name": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},

			"instance_id": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},

			"volume_id": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},

			"force_detach": {
				Type:     schema.TypeBool,
				Optional: true,
				Computed: true,
			},
			"skip_destroy": {
				Type:     schema.TypeBool,
				Optional: true,
				Computed: true,
			},
		},
	}
}

func resourceAwsVolumeAttachmentCreate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).ec2conn
	name := d.Get("device_name").(string)
	iID := d.Get("instance_id").(string)
	vID := d.Get("volume_id").(string)

	opts := &ec2.AttachVolumeInput{
		Device:     aws.String(name),
		InstanceId: aws.String(iID),
		VolumeId:   aws.String(vID),
	}

	log.Printf("[DEBUG] Attaching Volume (%s) to Instance (%s)", vID, iID)
	_, err := conn.AttachVolume(opts)
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			return fmt.Errorf("[WARN] Error attaching volume (%s) to instance (%s), message: \"%s\", code: \"%s\"",
				vID, iID, awsErr.Message(), awsErr.Code())
		}
		return err
	}

	stateConf := &resource.StateChangeConf{
		Pending:    []string{"attaching"},
		Target:     []string{"attached"},
		Refresh:    volumeAttachmentStateRefreshFunc(conn, vID, iID),
		Timeout:    5 * time.Minute,
		Delay:      10 * time.Second,
		MinTimeout: 3 * time.Second,
	}

	_, err = stateConf.WaitForState()
	if err != nil {
		return fmt.Errorf(
			"Error waiting for Volume (%s) to attach to Instance: %s, error: %s",
			vID, iID, err)
	}

	d.SetId(volumeAttachmentID(name, vID, iID))
	return resourceAwsVolumeAttachmentRead(d, meta)
}

func volumeAttachmentStateRefreshFunc(conn *ec2.EC2, volumeID, instanceID string) resource.StateRefreshFunc {
	return func() (interface{}, string, error) {

		request := &ec2.DescribeVolumesInput{
			VolumeIds: []*string{aws.String(volumeID)},
			Filters: []*ec2.Filter{
				&ec2.Filter{
					Name:   aws.String("attachment.instance-id"),
					Values: []*string{aws.String(instanceID)},
				},
			},
		}

		resp, err := conn.DescribeVolumes(request)
		if err != nil {
			if awsErr, ok := err.(awserr.Error); ok {
				return nil, "failed", fmt.Errorf("code: %s, message: %s", awsErr.Code(), awsErr.Message())
			}
			return nil, "failed", err
		}

		if len(resp.Volumes) > 0 {
			v := resp.Volumes[0]
			for _, a := range v.Attachments {
				if a.InstanceId != nil && *a.InstanceId == instanceID {
					return a, *a.State, nil
				}
			}
		}
		// assume detached if volume count is 0
		return 42, "detached", nil
	}
}
func resourceAwsVolumeAttachmentRead(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).ec2conn

	request := &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(d.Get("volume_id").(string))},
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name:   aws.String("attachment.instance-id"),
				Values: []*string{aws.String(d.Get("instance_id").(string))},
			},
		},
	}

	vols, err := conn.DescribeVolumes(request)
	if err != nil {
		if ec2err, ok := err.(awserr.Error); ok && ec2err.Code() == "InvalidVolume.NotFound" {
			d.SetId("")
			return nil
		}
		return fmt.Errorf("Error reading EC2 volume %s for instance: %s: %#v", d.Get("volume_id").(string), d.Get("instance_id").(string), err)
	}

	if len(vols.Volumes) == 0 || *vols.Volumes[0].State == "available" {
		log.Printf("[DEBUG] Volume Attachment (%s) not found, removing from state", d.Id())
		d.SetId("")
	}

	return nil
}

// InstanceStateRefreshFunc returns a resource.StateRefreshFunc that is used to watch
// an EC2 instance.
func InstanceStateRefreshFunc2(conn *ec2.EC2, instanceID string) resource.StateRefreshFunc {
	return func() (interface{}, string, error) {
		resp, err := conn.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		if err != nil {
			if ec2err, ok := err.(awserr.Error); ok && ec2err.Code() == "InvalidInstanceID.NotFound" {
				// Set this to nil as if we didn't find anything.
				resp = nil
			} else {
				log.Printf("Error on InstanceStateRefresh: %s", err)
				return nil, "", err
			}
		}

		if resp == nil || len(resp.Reservations) == 0 || len(resp.Reservations[0].Instances) == 0 {
			// Sometimes AWS just has consistency issues and doesn't see
			// our instance yet. Return an empty state.
			return nil, "", nil
		}

		i := resp.Reservations[0].Instances[0]
		return i, *i.State.Name, nil
	}
}

func resourceAwsVolumeAttachmentDelete(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).ec2conn

	if _, ok := d.GetOk("skip_destroy"); ok {
		log.Printf("[INFO] Found skip_destroy to be true, removing attachment %q from state", d.Id())
		d.SetId("")
		return nil
	}

	vID := d.Get("volume_id").(string)
	iID := d.Get("instance_id").(string)

	instance_stop_opts := &ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(iID)},
	}

	_, err := conn.StopInstances(instance_stop_opts)

	if err == nil {
		instanceStateConf := &resource.StateChangeConf{
			Pending:    []string{"stopping"},
			Target:     []string{"stopped"},
			Refresh:    InstanceStateRefreshFunc2(conn, iID),
			Timeout:    10 * time.Minute,
			Delay:      10 * time.Second,
			MinTimeout: 3 * time.Second,
		}
		log.Printf("[DEBUG] Stopping instance (%s)", iID)
		_, err = instanceStateConf.WaitForState()
		if err != nil {
			return fmt.Errorf(
				"Error waiting for Instance: %s to stop",
				iID)
		}
	}

	opts := &ec2.DetachVolumeInput{
		Device:     aws.String(d.Get("device_name").(string)),
		InstanceId: aws.String(iID),
		VolumeId:   aws.String(vID),
		Force:      aws.Bool(d.Get("force_detach").(bool)),
	}

	_, err = conn.DetachVolume(opts)
	stateConf := &resource.StateChangeConf{
		Pending:    []string{"detaching"},
		Target:     []string{"detached"},
		Refresh:    volumeAttachmentStateRefreshFunc(conn, vID, iID),
		Timeout:    5 * time.Minute,
		Delay:      10 * time.Second,
		MinTimeout: 3 * time.Second,
	}

	log.Printf("[DEBUG] Detaching Volume (%s) from Instance (%s)", vID, iID)
	_, err = stateConf.WaitForState()
	if err != nil {
		return fmt.Errorf(
			"Error waiting for Volume (%s) to detach from Instance: %s",
			vID, iID)
	}
	d.SetId("")
	return nil
}

func volumeAttachmentID(name, volumeID, instanceID string) string {
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("%s-", name))
	buf.WriteString(fmt.Sprintf("%s-", instanceID))
	buf.WriteString(fmt.Sprintf("%s-", volumeID))

	return fmt.Sprintf("vai-%d", hashcode.String(buf.String()))
}
