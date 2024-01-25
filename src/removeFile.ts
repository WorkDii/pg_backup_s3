import { $ } from "zx";
import { env } from "./env.ts";

export const removeFile = async (database: string) => {
  await $`rm -rf ${database}.dump`;
};
