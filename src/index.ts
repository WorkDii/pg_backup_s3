import { $ } from "zx";
import { env } from "./env.ts";
import { CronJob } from "cron";

async function main() {
  await $`PGPASSWORD=${env.POSTGRES_PASSWORD} pg_dump --format=custom \
  -h ${env.POSTGRES_HOST} \
  -p ${env.POSTGRES_PORT} \
  -U ${env.POSTGRES_USER} \
  -d ${env.POSTGRES_DATABASE} \
  > ${env.POSTGRES_DATABASE}.dump`;
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
