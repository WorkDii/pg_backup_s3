import { $ } from "zx";
import { env } from "./env.js";

export const dumpFile = async (database: string) => {
  await $`PGPASSWORD=${env.POSTGRES_PASSWORD} pg_dump --format=custom \
  -h ${env.POSTGRES_HOST} \
  -p ${env.POSTGRES_PORT} \
  -U ${env.POSTGRES_USER} \
  -d ${database} \
  > ${database}.dump`;
};
