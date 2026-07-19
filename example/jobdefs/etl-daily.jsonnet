local image = '{{ tfstate `aws_ecr_repository.example.repository_url` }}';
local tag = '{{ env `IMAGE_TAG` `latest` }}';

{
  jobDefinitionName: 'etl-daily',
  type: 'container',
  platformCapabilities: ['FARGATE'],
  containerProperties: {
    image: image + ':' + tag,
    command: ['python', 'etl.py', '--date', 'Ref::date'],
    jobRoleArn: '{{ tfstate `aws_iam_role.batch_job.arn` }}',
    executionRoleArn: '{{ tfstate `aws_iam_role.batch_job.arn` }}',
    resourceRequirements: [
      { type: 'VCPU', value: '1' },
      { type: 'MEMORY', value: '2048' },
    ],
    environment: [
      { name: 'TZ', value: 'Asia/Tokyo' },
    ],
    logConfiguration: {
      logDriver: 'awslogs',
      options: {
        'awslogs-group': '/aws/batch/job',
        'awslogs-stream-prefix': 'etl-daily',
      },
    },
  },
  parameters: {
    date: '',
  },
  retryStrategy: {
    attempts: 3,
    evaluateOnExit: [
      { onStatusReason: 'Host EC2*', action: 'RETRY' },
      { onReason: '*', action: 'EXIT' },
    ],
  },
  timeout: {
    attemptDurationSeconds: 3600,
  },
  tags: {
    ManagedBy: 'batchbru',
  },
}
