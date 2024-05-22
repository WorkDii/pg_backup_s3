import { S3 } from "@aws-sdk/client-s3";
import { env } from "./env.js";
import { createReadStream } from "fs";
import { addDays } from "date-fns";

const {
  S3_BUCKET,
  S3_ENDPOINT,
  S3_ACCESS_KEY_ID,
  S3_REGION,
  S3_SECRET_ACCESS_KEY,
} = env;

// Extracted the common logic of creating a S3 client into a separate function
const createS3Client = () => {
  return new S3({
    forcePathStyle: false, // Configures to use subdomain/virtual calling format.
    endpoint: S3_ENDPOINT,
    region: S3_REGION,
    credentials: {
      accessKeyId: S3_ACCESS_KEY_ID,
      secretAccessKey: S3_SECRET_ACCESS_KEY,
    },
  });
};

export const s3Client = createS3Client();

export const uploadToS3 = async ({
  name,
  path,
}: {
  name: string;
  path: string;
}) => {
  await s3Client.putObject({
    Bucket: S3_BUCKET,
    Key: name,
    Body: createReadStream(path),
  });
};

export const removeOldS3 = async (keepDay: number, folder: string) => {
  const list = await s3Client.listObjectsV2({
    Bucket: S3_BUCKET,
    Prefix: folder,
  });
  const keepDate = addDays(new Date(), -keepDay);
  const objectsToDelete = list.Contents?.filter((file) => {
    const fileDate = new Date(file.LastModified as any);
    return fileDate < keepDate;
  });

  if (objectsToDelete) {
    await s3Client.deleteObjects({
      Bucket: S3_BUCKET,
      Delete: {
        Objects: objectsToDelete.map((file) => ({ Key: file.Key! })),
      },
    });
  }
};

export const uploads3AndRemoveOldS3 = async ({
  database,
  keepDay,
  scheduleName,
  timestamp,
}: {
  keepDay: number;
  database: string;
  scheduleName: string;
  timestamp: string;
}) => {
  const folder = `db_backup/${database}/${scheduleName}`;
  const filename = `${folder}/${database}-${timestamp}.dump`;
  await uploadToS3({ name: filename, path: `${database}.dump` });
  await removeOldS3(keepDay, folder);
};
