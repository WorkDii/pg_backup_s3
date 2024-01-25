import { $ } from "zx";
import { env } from "./env.ts";
import { CronJob } from "cron";
import { S3Client, S3ClientConfig, S3 } from "@aws-sdk/client-s3";
import { Upload } from "@aws-sdk/lib-storage";
import { createReadStream, unlink, statSync } from "fs";
import { s3Client } from "./s3.ts";
import { dumpFile } from "./dumpFile.ts";
import { removeFile } from "./removeFile.ts";

const uploadToS3 = async ({ name, path }: { name: string; path: string }) => {
  console.log("Uploading backup to S3...");

  const bucket = env.S3_BUCKET;

  await s3Client.putObject({
    Bucket: bucket,
    Key: name,
    Body: createReadStream(path),
  });
  console.log("Backup uploaded to S3...");
};

async function main() {
  const database = env.POSTGRES_DATABASE;
  const timestamp = new Date().toISOString();
  const folder = `${database}/hourly`;
  const filename = `${folder}/${database}-${timestamp}.dump`;

  await dumpFile(database);
  await uploadToS3({ name: filename, path: `${database}.dump` });
  await removeFile(database);
}
main();

// if (env.SCHEDULE_HOURLY && env.BACKUP_KEEP_DAYS_HOURLY) {
//   const job = new CronJob(env.SCHEDULE_HOURLY, async () => {
//     await $`pg_dump --format=custom \
//     -h ${env.POSTGRES_HOST} \
//     -p ${env.POSTGRES_PORT} \
//     -U ${env.POSTGRES_USER} \
//     -d ${env.POSTGRES_DATABASE} \
//     > ${env.POSTGRES_DATABASE}.dump`;
//   });
//   job.start();
// }
