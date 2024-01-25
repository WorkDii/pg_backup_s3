import { load } from "ts-dotenv";

export const env = load({
  S3_REGION: String,
  S3_ACCESS_KEY_ID: String,
  S3_SECRET_ACCESS_KEY: String,
  S3_BUCKET: String,
  S3_ENDPOINT: String,

  POSTGRES_HOST: String,
  POSTGRES_PORT: Number,
  POSTGRES_USER: String,
  POSTGRES_PASSWORD: String,
  POSTGRES_DATABASE: String,

  SCHEDULE_MONTHLY: {
    type: String,
    optional: true,
  },
  BACKUP_KEEP_DAYS_MONTHLY: {
    type: Number,
    optional: true,
  },

  SCHEDULE_WEEKLY: {
    type: String,
    optional: true,
  },
  BACKUP_KEEP_DAYS_WEEKLY: {
    type: Number,
    optional: true,
  },

  SCHEDULE_DAILY: {
    type: String,
    optional: true,
  },
  BACKUP_KEEP_DAYS_DAILY: {
    type: Number,
    optional: true,
  },
  SCHEDULE_HOURLY: {
    type: String,
    optional: true,
  },
  BACKUP_KEEP_DAYS_HOURLY: {
    type: Number,
    optional: true,
  },
});
