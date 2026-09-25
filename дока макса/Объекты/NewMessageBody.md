# NewMessageBody

Объект используется при отправке нового сообщения в чате или канале[`POST /messages`](https://dev.max.ru/docs-api/methods/POST/messages), редактировании существующего сообщения или поста [`PUT /messages`](https://dev.max.ru/docs-api/methods/PUT/messages), а также callback-отправке сообщения пользователю при нажатии им кнопки в чате или канале [`POST /answers`](https://dev.max.ru/docs-api/methods/POST/answers)

Параметры объекта содержат: текст и способ его форматирования, вложения, ссылку на связанное сообщение (ответ или пересылка), настройку PUSH-уведомлений для участников чата

`text`  
string Nullable  

до `4000` символов

`attachments`  
AttachmentRequest[] Nullable  

Вложения сообщения. Если поле равно `null`, изменений не произойдет. Если массив пуст, все вложения будут удалены

`link`  
object NewMessageLink Nullable

Ссылка на сообщение

`notify`  
boolean optional  

По умолчанию: `true`

Если `false`, участники чата не получат push-уведомления. Для каналов необходимо отправлять запрос с `notify = true` или без этого поля, т.к. каналы не подразумевают отправку постов без push-уведомлений

`format`  
enum TextFormat Nullable optional

Возможные значения в enum: `"markdown"` `"html"`

Если установлен, текст сообщения будет форматирован данным способом. Для подробной информации загляните в раздел [Форматирование](https://dev.max.ru/docs-api#%D0%A4%D0%BE%D1%80%D0%BC%D0%B0%D1%82%D0%B8%D1%80%D0%BE%D0%B2%D0%B0%D0%BD%D0%B8%D0%B5%20%D1%82%D0%B5%D0%BA%D1%81%D1%82%D0%B0%20%D0%B2%20%D1%81%D0%BE%D0%BE%D0%B1%D1%89%D0%B5%D0%BD%D0%B8%D1%8F%D1%85)

## [](https://dev.max.ru/docs-api/objects/NewMessageBody#%D0%9F%D1%80%D0%B8%D0%BC%D0%B5%D1%80%20%D0%BE%D0%B1%D1%8A%D0%B5%D0%BA%D1%82%D0%B0)Пример объекта

JSON

`{ "text": "string", "attachments": [{ ... }], "link": { ... }, "notify": true, "format": "markdown" }`