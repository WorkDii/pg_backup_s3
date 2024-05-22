import { env } from "./env.js";
import { CronJob } from "cron";
import { removeOldS3, uploadToS3, uploads3AndRemoveOldS3 } from "./s3.js";
import { dumpFile } from "./dumpFile.js";
import { removeLocalFile } from "./removeFile.js";
import pMap from "p-map";

const {
  POSTGRES_DATABASE,
  SCHEDULE_HOURLY,
  BACKUP_KEEP_DAYS_HOURLY,
  BACKUP_KEEP_DAYS_DAILY,
  BACKUP_KEEP_DAYS_WEEKLY,
  BACKUP_KEEP_DAYS_MONTHLY,
} = env;

const logAndExecute = async (message: string, task: () => Promise<void>) => {
  console.log(`Starting ${message} ...`);
  await task();
  console.log(`${message} done...`);
};

const runningJob = async (database: string) => {
  console.log(`${new Date().toISOString()} Backup ${database} starting...`);

  const timestamp = new Date().toISOString();

  await logAndExecute(`dump file ${database}`, () => dumpFile(database));

  await logAndExecute(`upload to s3 ${database} hourly`, () => {
    return uploads3AndRemoveOldS3({
      database,
      keepDay: BACKUP_KEEP_DAYS_HOURLY,
      scheduleName: "hourly",
      timestamp,
    });
  });

  // daily
  if (BACKUP_KEEP_DAYS_DAILY && new Date().getHours() === 22) {
    await logAndExecute(`upload to s3 ${database} daily`, () => {
      return uploads3AndRemoveOldS3({
        database,
        keepDay: BACKUP_KEEP_DAYS_DAILY,
        scheduleName: "daily",
        timestamp,
      });
    });
  }

  // weekly
  if (
    BACKUP_KEEP_DAYS_WEEKLY &&
    new Date().getDay() === 0 &&
    new Date().getHours() === 22
  ) {
    await logAndExecute(`upload to s3 ${database} weekly`, () => {
      return uploads3AndRemoveOldS3({
        database,
        keepDay: BACKUP_KEEP_DAYS_WEEKLY,
        scheduleName: "weekly",
        timestamp,
      });
    });
  }

  // monthly
  if (
    BACKUP_KEEP_DAYS_MONTHLY &&
    new Date().getDate() === 1 &&
    new Date().getHours() === 22
  ) {
    await logAndExecute(`upload to s3 ${database} monthly`, () => {
      return uploads3AndRemoveOldS3({
        database,
        keepDay: BACKUP_KEEP_DAYS_MONTHLY,
        scheduleName: "monthly",
        timestamp,
      });
    });
  }

  await logAndExecute(`remove local file ${database}`, () =>
    removeLocalFile(database)
  );

  console.log(`${new Date().toISOString()} Backup ${database} done...`);
};

const job = new CronJob(SCHEDULE_HOURLY, async () => {
  const allDatabase = POSTGRES_DATABASE.split(",");
  await pMap(
    allDatabase,
    async (database) => {
      await runningJob(database.trim());
    },
    { concurrency: 1 }
  );
});

console.log(`Starting job backup database`);
job.start();
