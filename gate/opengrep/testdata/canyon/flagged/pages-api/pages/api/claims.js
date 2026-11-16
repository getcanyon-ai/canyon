import { db } from "@/lib/db";

export default async function handler(req, res) {
  const rows = await db.query(`SELECT * FROM claims WHERE id = ${req.query.id}`);
  res.status(200).json(rows);
}
