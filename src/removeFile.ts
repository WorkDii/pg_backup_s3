import { $ } from "zx";
import { env } from "./env.ts";

export const removeLocalFile = async (database: string) => {
  await $`rm -rf ${database}.dump`;
};
