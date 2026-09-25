# BotInfo

Объект включает общую информацию о боте, URL аватара и описание. Является наследником [схемы UserWithPhoto](https://dev.max.ru/docs-api/objects/UserWithPhoto). Дополнительно к ней содержит список команд, поддерживаемых ботом. Возвращается только при вызове метода `GET /me`

`user_id`  
integer `<int64>  

Идентификатор пользователя или бота

`first_name`  
string  

Отображаемое имя пользователя или бота

`last_name`  
string Nullable optional  

Отображаемая фамилия пользователя. Для ботов это поле не возвращается

`username`  
string Nullable  

Никнейм бота или уникальное публичное имя пользователя. В случае с пользователем может быть `null`, если тот недоступен или имя не задано

`is_bot`  
boolean  

`true`, если это бот

`last_activity_time`  
integer `<int64>  

Время последней активности пользователя или бота в MAX (Unix timestamp в миллисекундах). Если пользователь отключил в настройках профиля мессенджера MAX возможность видеть, что он в сети онлайн, поле может не возвращаться

`description`  
string Nullable optional  

до `16000` символов

Описание пользователя или бота. В случае с пользователем может принимать значение `null`, если описание не заполнено

`avatar_url`  
string optional  

URL аватара пользователя или бота в уменьшенном размере

`full_avatar_url`  
string optional  

URL аватара пользователя или бота в полном размере

`commands`  
BotCommand[] Nullable optional  

до `32` элементов

Команды, поддерживаемые ботом

## [](https://dev.max.ru/docs-api/objects/BotInfo#%D0%9F%D1%80%D0%B8%D0%BC%D0%B5%D1%80%20%D0%BE%D0%B1%D1%8A%D0%B5%D0%BA%D1%82%D0%B0)Пример объекта

JSON

`{ "user_id": 0, "first_name": "string", "last_name": "string", "username": "string", "is_bot": true, "last_activity_time": 0, "name": "string", "description": "string", "avatar_url": "string", "full_avatar_url": "string", "commands": [{ ... }] }`