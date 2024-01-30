import { env } from "./env.js";
import { CronJob } from "cron";
import { removeOldS3, uploadToS3 } from "./s3.js";
import { dumpFile } from "./dumpFile.js";
import { removeLocalFile } from "./removeFile.js";

const {
  POSTGRES_DATABASE,
  SCHEDULE_HOURLY,
  BACKUP_KEEP_DAYS_HOURLY,
  SCHEDULE_DAILY,
  BACKUP_KEEP_DAYS_DAILY,
  SCHEDULE_WEEKLY,
  BACKUP_KEEP_DAYS_WEEKLY,
  SCHEDULE_MONTHLY,
  BACKUP_KEEP_DAYS_MONTHLY,
} = env;

const logAndExecute = async (message: string, task: () => Promise<void>) => {
  console.log(`Starting ${message} ...`);
  await task();
  console.log(`${message} done...`);
};

const runningJob = async (database: string, keepDay: number) => {
  console.log(`${new Date().toISOString()} Backup ${database} starting...`);

  const timestamp = new Date().toISOString();
  const folder = `db_backup/${database}/hourly`;
  const filename = `${folder}/${database}-${timestamp}.dump`;

  await logAndExecute(`dump file ${database}`, () => dumpFile(database));
  await logAndExecute(`upload to S3 ${database}`, () =>
    uploadToS3({ name: filename, path: `${database}.dump` })
  );
  await logAndExecute(`remove local file ${database}`, () =>
    removeLocalFile(database)
  );
  await logAndExecute(`remove old S3 ${database}`, () =>
    removeOldS3(keepDay, folder)
  );

  console.log(`${new Date().toISOString()} Backup ${database} done...`);
};

async function createJob(schedule: string, keepDays: number, jobName: string) {
  const job = new CronJob(schedule, async () => {
    const all = POSTGRES_DATABASE.split(",").map((database) => {
      return runningJob(database.trim(), keepDays || 1);
    });
    await Promise.all(all);
  });

  console.log(`Starting ${jobName}...`);
  job.start();
}

if (!!SCHEDULE_HOURLY && !!BACKUP_KEEP_DAYS_HOURLY) {
  createJob(SCHEDULE_HOURLY, BACKUP_KEEP_DAYS_HOURLY, "job hourly");
}

if (!!SCHEDULE_DAILY && !!BACKUP_KEEP_DAYS_DAILY) {
  createJob(SCHEDULE_DAILY, BACKUP_KEEP_DAYS_DAILY, "job daily");
}

if (!!SCHEDULE_WEEKLY && !!BACKUP_KEEP_DAYS_WEEKLY) {
  createJob(SCHEDULE_WEEKLY, BACKUP_KEEP_DAYS_WEEKLY, "job weekly");
}

if (!!SCHEDULE_MONTHLY && !!BACKUP_KEEP_DAYS_MONTHLY) {
  createJob(SCHEDULE_MONTHLY, BACKUP_KEEP_DAYS_MONTHLY, "job monthly");
}
